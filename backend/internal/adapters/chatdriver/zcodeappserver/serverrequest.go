package zcodeappserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/zcodeappserver/zcodeproto"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// approvalWait bounds how long a parked server request is left unanswered. No
// server-side timeout exists for permission or user-input requests, so an
// approval nobody resolves would hang the turn indefinitely; on expiry AO
// refuses — to every waiting frame id — rather than deciding for the user.
const approvalWait = 30 * time.Minute

// recentlyResolvedLimit bounds the memory of answered requests kept to answer a
// re-announce that crossed the reply on the wire.
const recentlyResolvedLimit = 64

// errConversationClosed reports a decision arriving after the controller ended.
var errConversationClosed = errors.New("conversation closed")

// parkedRequest is one server->client request waiting on a person, shared by
// every frame id the server re-announced it under (DESIGN 3.3, 3.5).
//
// The server re-announces an unanswered interaction request with a fresh
// `server-N` id every 1 s, doubling to 10 s. Each re-announce reaches the
// transport as a separate answer goroutine; every one of them joins this
// record and returns the same reply, so conn.answer writes exactly one reply
// per frame id. That matters beyond tidiness: the raw persistent host keys its
// pending table by frame id and replays every id that never got a reply to the
// next daemon, which would park a ghost approval on an idle session.
type parkedRequest struct {
	method     string
	requestID  string
	toolCallID string
	turnID     string
	// offered keeps the provider's own response object per option id so the
	// reply echoes it verbatim: the server zod-parses the result strictly.
	offered map[string]json.RawMessage
	// done closes once reply or replyErr is set. waiters counts the frame ids
	// parked on this record, for diagnostics.
	done     chan struct{}
	reply    any
	replyErr error
	waiters  int
	// deferred is set while a live reconnect has not yet reconciled the
	// host-replayed requests against the server's pending list; the
	// approval.requested event is emitted (or the request refused) by
	// reconcilePending rather than by the handler.
	deferred bool
	emit     ports.ChatEvent
}

// resolve settles every waiter with one answer. Safe to call once; later calls
// are no-ops so a timeout racing a decision cannot double-close.
func (p *parkedRequest) resolve(reply any, err error) bool {
	select {
	case <-p.done:
		return false
	default:
	}
	p.reply = reply
	p.replyErr = err
	close(p.done)
	return true
}

func (p *parkedRequest) offeredIDs() []string {
	ids := make([]string, 0, len(p.offered))
	for id := range p.offered {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// handleServerRequest is the router of DESIGN 3.5, one case per method. The
// provider blocks on the reply for interaction requests, so those park rather
// than answer. Everything AO does not model is refused with -32601, never with
// a fabricated decision.
func (c *conversation) handleServerRequest(ctx context.Context, req serverRequest) (any, error) {
	switch req.Method {
	case zcodeproto.MethodSessionRequestRuntimePreferences:
		// Never refused: this is raised inside session/create and a refusal other
		// than -32601/-32020 fails the create. Both keys are always sent; the
		// second one off keeps the server from self-answering AskUserQuestion.
		c.observeRuntimePreferences(req.Params)
		return zcodeproto.RequestRuntimePreferencesResult{
			NativeSearchEnhancementsEnabled:      true,
			AskUserQuestionAutoResolutionEnabled: false,
		}, nil

	case zcodeproto.MethodInteractionRequestPermission:
		return c.parkPermission(ctx, req)

	case zcodeproto.MethodInteractionRequestUserInput:
		return c.parkUserInput(ctx, req)

	case zcodeproto.MethodInteractionRequestProviderRuntimeHeaders:
		var params zcodeproto.RequestProviderRuntimeHeadersParams
		_ = json.Unmarshal(req.Params, &params)
		resolver := c.runtimeHeaders
		if resolver == nil {
			resolver = declineResolver{}
		}
		result, err := resolver.Resolve(ctx, params)
		if err != nil {
			// Logged without the params: the request names the account and the
			// reply would carry credentials. Neither belongs in a log line.
			c.log.Warn("declined provider runtime headers request", "reason", params.Reason)
			return nil, err
		}
		return result, nil

	case zcodeproto.MethodInteractionRequestOfficialMcpAuthHeaders:
		// AO holds no MCP auth for zcode's official servers; the server's
		// resolveHeaders catch-all turns any error into official_auth_unavailable.
		return nil, &rpcError{Code: zcodeproto.CodeClientDeclined, Message: "declined by agent-orchestrator"}

	case zcodeproto.MethodSubagentsReadRuntimeConfig:
		// Only sent when the client enabled subagent runtime config, which AO
		// does not; answered with the built-in fallback if it ever arrives.
		return zcodeproto.SubagentRuntimeConfigResult{Kind: "built-in-fallback"}, nil
	}

	c.log.Warn("refusing unmodelled app-server request", "method", req.Method)
	return nil, &rpcError{Code: zcodeproto.CodeMethodNotFound, Message: fmt.Sprintf("unsupported request %s", req.Method)}
}

// observeRuntimePreferences records the session id the server named inside
// session/create. That request's params.sessionId is the PROVEN id source.
func (c *conversation) observeRuntimePreferences(params json.RawMessage) {
	var p zcodeproto.RequestRuntimePreferencesParams
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
		return
	}
	c.mu.Lock()
	if c.preferencesSessionID == "" {
		c.preferencesSessionID = p.SessionID
	}
	c.mu.Unlock()
}

// parkPermission parks interaction/requestPermission until ResolveRequest.
func (c *conversation) parkPermission(ctx context.Context, req serverRequest) (any, error) {
	var params zcodeproto.RequestPermissionParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.RequestID == "" {
		return nil, &rpcError{Code: zcodeproto.CodeInvalidParams, Message: "permission request carried no requestId"}
	}
	decisions := decisionOptions(params.Options)
	offered := make(map[string]json.RawMessage, len(decisions))
	for _, option := range decisions {
		offered[option.ID] = option.Raw
	}
	summary := params.ToolName
	if params.Reason != "" {
		if summary != "" {
			summary += ": "
		}
		summary += params.Reason
	}
	if summary == "" {
		summary = "Approval required"
	}
	detail, _ := json.Marshal(map[string]any{
		"toolName": params.ToolName, "riskLevel": params.RiskLevel,
		"input": json.RawMessage(validOr(params.Input, []byte("null"))), "toolCallId": params.ToolCallID,
	})
	ev := ports.ChatEvent{
		Kind:           ports.ChatEventApprovalRequested,
		ProviderTurnID: c.turnForRequest(params.TurnID),
		ProviderItemID: params.RequestID,
		RequestID:      params.RequestID,
		ActivityKind:   approvalKind(params.ToolName),
		ActivityStatus: domain.ActivityStatusPending,
		Summary:        summary,
		Detail:         detail,
		Decisions:      decisions,
	}
	return c.park(ctx, req, params.RequestID, params.ToolCallID, params.TurnID, offered, ev)
}

// parkUserInput parks interaction/requestUserInput until ResolveInput.
func (c *conversation) parkUserInput(ctx context.Context, req serverRequest) (any, error) {
	var params zcodeproto.RequestUserInputParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.RequestID == "" {
		return nil, &rpcError{Code: zcodeproto.CodeInvalidParams, Message: "user input request carried no requestId"}
	}
	ev := ports.ChatEvent{
		Kind:           ports.ChatEventInputRequested,
		ProviderTurnID: c.turnForRequest(params.TurnID),
		ProviderItemID: params.RequestID,
		RequestID:      params.RequestID,
		ActivityKind:   domain.ActivityKindUserInput,
		ActivityStatus: domain.ActivityStatusPending,
		Summary:        inputRequestFrom(params).Message,
		Input:          inputRequestFrom(params),
	}
	return c.park(ctx, req, params.RequestID, "", params.TurnID, nil, ev)
}

// park registers (or joins) the parked record for a requestId and waits for its
// answer. Exactly one approval.requested/input.requested is emitted per
// requestId while every frame id is answered.
func (c *conversation) park(ctx context.Context, req serverRequest, requestID, toolCallID, turnID string,
	offered map[string]json.RawMessage, ev ports.ChatEvent) (any, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, &rpcError{Code: zcodeproto.CodeMethodNotFound, Message: errConversationClosed.Error()}
	}
	if reply, ok := c.recentlyResolved[requestID]; ok {
		// A re-announce that crossed the reply on the wire: answer it the same
		// way instead of parking a second approval for a decision already made.
		c.mu.Unlock()
		return reply, nil
	}
	parked, joined := c.pending[requestID]
	if joined {
		parked.waiters++
		c.mu.Unlock()
		return c.awaitParked(ctx, parked)
	}
	parked = &parkedRequest{
		method: req.Method, requestID: requestID, toolCallID: toolCallID, turnID: turnID,
		offered: offered, done: make(chan struct{}), waiters: 1,
	}
	c.pending[requestID] = parked
	if toolCallID != "" {
		c.byToolCallID[toolCallID] = requestID
	}
	deferred := c.reconciling
	if deferred {
		parked.deferred = true
		parked.emit = ev
	}
	c.mu.Unlock()

	if !deferred {
		c.emit(ev)
	}

	timer := time.NewTimer(c.approvalWait)
	defer timer.Stop()
	select {
	case <-parked.done:
		return parked.reply, parked.replyErr
	case <-timer.C:
		// Expiry refuses every waiting id rather than deciding; the record is
		// dropped so a later decision gets ErrChatRequestNotPending.
		c.refusePending(requestID, &rpcError{Code: zcodeproto.CodeMethodNotFound, Message: "approval timed out without a decision"})
		return parked.reply, parked.replyErr
	case <-ctx.Done():
		c.refusePending(requestID, ctx.Err())
		return nil, ctx.Err()
	}
}

// awaitParked is the path a re-announced frame id takes: wait for the shared
// answer and return it so its own reply frame is written.
func (c *conversation) awaitParked(ctx context.Context, parked *parkedRequest) (any, error) {
	select {
	case <-parked.done:
		return parked.reply, parked.replyErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// refusePending answers every waiter of a request with an error and forgets it.
func (c *conversation) refusePending(requestID string, err error) {
	c.mu.Lock()
	parked, ok := c.pending[requestID]
	if ok {
		delete(c.pending, requestID)
		if parked.toolCallID != "" {
			delete(c.byToolCallID, parked.toolCallID)
		}
	}
	c.mu.Unlock()
	if ok {
		parked.resolve(nil, err)
	}
}

// ResolveRequest answers a parked permission request with one of the decisions
// the provider offered for it. The reply is the option's response object
// verbatim, written to every frame id the request was announced under.
func (c *conversation) ResolveRequest(ctx context.Context, requestID string, decision ports.ChatDecision) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errConversationClosed
	}
	parked, ok := c.pending[requestID]
	if !ok {
		c.mu.Unlock()
		// Already resolved, superseded, or from a previous controller. A stale
		// card must never resolve a newer request.
		return fmt.Errorf("%w: %q", ports.ErrChatRequestNotPending, requestID)
	}
	if parked.method != zcodeproto.MethodInteractionRequestPermission {
		c.mu.Unlock()
		return fmt.Errorf("%w: %q expects structured input, not a decision", ports.ErrChatDecisionNotOffered, requestID)
	}
	raw, offered := parked.offered[decision.ID]
	if !offered {
		c.mu.Unlock()
		// Refused and left pending: consent AO invented is not consent, and
		// consuming the request would leave the real answer nothing to answer.
		return fmt.Errorf("%w: %q (offered: %s)",
			ports.ErrChatDecisionNotOffered, decision.ID, strings.Join(parked.offeredIDs(), ", "))
	}
	c.settleLocked(parked, json.RawMessage(raw))
	c.mu.Unlock()
	return ctx.Err()
}

// ResolveInput answers a parked interaction/requestUserInput with the user's
// structured response (ports.ChatInputResponder).
func (c *conversation) ResolveInput(ctx context.Context, requestID string, response ports.ChatInputResponse) error {
	if !response.Action.Valid() {
		return fmt.Errorf("invalid input action %q", response.Action)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errConversationClosed
	}
	parked, ok := c.pending[requestID]
	if !ok {
		c.mu.Unlock()
		return fmt.Errorf("%w: %q", ports.ErrChatRequestNotPending, requestID)
	}
	if parked.method != zcodeproto.MethodInteractionRequestUserInput {
		c.mu.Unlock()
		return fmt.Errorf("%w: %q expects a decision, not structured input", ports.ErrChatRequestNotPending, requestID)
	}
	reply := zcodeproto.UserInputResponse{Action: string(response.Action)}
	if response.Action == ports.ChatInputActionAccept && response.Content != nil {
		reply.Content = response.Content
	}
	c.settleLocked(parked, reply)
	c.mu.Unlock()
	return ctx.Err()
}

// settleLocked answers a parked request and remembers the reply for any
// re-announce still in flight. Caller holds c.mu.
func (c *conversation) settleLocked(parked *parkedRequest, reply any) {
	delete(c.pending, parked.requestID)
	if parked.toolCallID != "" {
		delete(c.byToolCallID, parked.toolCallID)
	}
	c.rememberResolvedLocked(parked.requestID, reply)
	parked.resolve(reply, nil)
}

func (c *conversation) rememberResolvedLocked(requestID string, reply any) {
	if len(c.recentlyResolved) >= recentlyResolvedLimit {
		// Evict arbitrarily: the map only needs to cover the re-announce window.
		for key := range c.recentlyResolved {
			delete(c.recentlyResolved, key)
			break
		}
	}
	c.recentlyResolved[requestID] = reply
}

// forgetResolved drops the remembered reply once the server reports the request
// resolved, after which it cannot re-announce.
func (c *conversation) forgetResolved(requestID string) {
	if requestID == "" {
		return
	}
	c.mu.Lock()
	delete(c.recentlyResolved, requestID)
	c.mu.Unlock()
}

// discardPending forgets a request the provider resolved on its own.
func (c *conversation) discardPending(requestID string) {
	c.mu.Lock()
	parked, ok := c.pending[requestID]
	if ok {
		delete(c.pending, requestID)
		if parked.toolCallID != "" {
			delete(c.byToolCallID, parked.toolCallID)
		}
	}
	c.mu.Unlock()
	if ok {
		// The server already settled it elsewhere; answering with a refusal
		// clears the frame ids without pretending to decide.
		parked.resolve(nil, &rpcError{Code: zcodeproto.CodeMethodNotFound, Message: "request was resolved by the provider"})
	}
}

// requestIDForToolCall translates a toolCallId to the parked requestId, for a
// permission.resolved that omits requestId.
func (c *conversation) requestIDForToolCall(toolCallID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byToolCallID[toolCallID]
}

// reconcilePending finishes a live reconnect (DESIGN 3.5/3.8): host-replayed
// server requests whose requestId the server still lists as pending are
// emitted once; the rest are refused with -32601 so a request the previous
// daemon already resolved never becomes a second approval.
func (c *conversation) reconcilePending(pendingRequestIDs []string, known bool) {
	still := make(map[string]struct{}, len(pendingRequestIDs))
	for _, id := range pendingRequestIDs {
		still[id] = struct{}{}
	}
	c.mu.Lock()
	c.reconciling = false
	var emit []ports.ChatEvent
	var refuse []*parkedRequest
	for id, parked := range c.pending {
		if !parked.deferred {
			continue
		}
		parked.deferred = false
		if _, live := still[id]; live || !known {
			// Without a server answer, keeping the request is the conservative
			// choice: a stale card is recoverable, a lost real approval is not.
			emit = append(emit, parked.emit)
			continue
		}
		delete(c.pending, id)
		if parked.toolCallID != "" {
			delete(c.byToolCallID, parked.toolCallID)
		}
		refuse = append(refuse, parked)
	}
	c.mu.Unlock()
	for _, parked := range refuse {
		parked.resolve(nil, &rpcError{Code: zcodeproto.CodeMethodNotFound, Message: "request is no longer pending on the server"})
	}
	for _, ev := range emit {
		c.emit(ev)
	}
}

// failPendingApprovals unblocks every parked handler when the controller ends.
// User-input requests are cancelled rather than refused (DESIGN 3.5).
func (c *conversation) failPendingApprovals() {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[string]*parkedRequest{}
	c.byToolCallID = map[string]string{}
	c.closed = true
	c.mu.Unlock()
	for _, parked := range pending {
		if parked.method == zcodeproto.MethodInteractionRequestUserInput {
			parked.resolve(zcodeproto.UserInputResponse{Action: string(ports.ChatInputActionCancel)}, nil)
			continue
		}
		parked.resolve(nil, &rpcError{Code: zcodeproto.CodeMethodNotFound, Message: errConversationClosed.Error()})
	}
}

// turnForRequest maps a request's native turn id onto AO's, falling back to the
// turn in flight.
func (c *conversation) turnForRequest(native string) string {
	return turnFor(c.turns, native)
}

// approvalKind classifies a permission request by the tool it guards.
func approvalKind(toolName string) domain.ActivityKind {
	if toolName == "" {
		return domain.ActivityKindApproval
	}
	switch kindForTool(toolName) {
	case domain.ActivityKindFileChange:
		return domain.ActivityKindFileChange
	case domain.ActivityKindMCPTool:
		return domain.ActivityKindApproval
	default:
		return domain.ActivityKindCommand
	}
}

func validOr(raw json.RawMessage, fallback json.RawMessage) json.RawMessage {
	if len(raw) > 0 && json.Valid(raw) {
		return raw
	}
	return fallback
}
