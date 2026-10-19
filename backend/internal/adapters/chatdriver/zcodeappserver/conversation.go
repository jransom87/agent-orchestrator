package zcodeappserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/zcodeappserver/zcodeproto"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// eventBuffer bounds the normalized event stream. Deltas are dropped when a
// consumer falls this far behind; lifecycle events are not.
const eventBuffer = 4096

// sendAckTimeout bounds the synchronous session/send acknowledgement inside
// StartDeferredTurn (about 1.3 s observed, HANDOFF.md). The ack is not the turn:
// the turn runs on after it and settles through notifications.
const sendAckTimeout = 60 * time.Second

// usagePollTimeout bounds the session/usage read after a turn settles (U9).
const usagePollTimeout = 5 * time.Second

// deliveryKind is the session/subscribe delivery mode that makes the server
// dispatch session/event frames (STATIC, U1 settled).
const deliveryKind = "desktop-continuous"

// preparedTurn is a SendTurn waiting for StartDeferredTurn.
type preparedTurn struct {
	id              string
	text            string
	clientMessageID string
	settings        ports.ChatTurnSettings
}

// conversation is one live zcode session. It is the only writer to that session.
type conversation struct {
	conn *conn
	proc *process
	log  *slog.Logger

	sessionID       string
	workspacePath   string
	providerScopeID string
	// launchMode is the permission mode the session was created or resumed
	// with; a per-turn approval setting that differs is refused (U5).
	launchMode ports.PermissionMode
	events     chan ports.ChatEvent

	mu      sync.Mutex
	pending map[string]*parkedRequest
	// byToolCallID indexes parked permission requests by toolCallId, because
	// permission.resolved may omit requestId (`Uhr`).
	byToolCallID map[string]string
	// recentlyResolved answers a re-announce that crossed the reply on the wire.
	recentlyResolved map[string]any
	closed           bool
	// reconciling is set while a live reconnect has not yet reconciled the
	// host-replayed server requests against the server's pending list.
	reconciling bool
	// preferencesSessionID is the session id named by the
	// session/requestRuntimePreferences request raised inside session/create —
	// the PROVEN id source.
	preferencesSessionID string
	snapshot             zcodeproto.Snapshot
	// interrupting is the turn a session/stop is in flight for, so its failed
	// settlement reads as interrupted.
	interrupting string

	// sendMu serializes turn dispatch so only one operation mutates the
	// provider session at a time.
	sendMu        sync.Mutex
	prepared      *preparedTurn
	turns         *turnTracker
	preamble      string
	preambleSpent bool

	runtimeHeaders runtimeHeadersResolver
	approvalWait   time.Duration
	sendTimeout    time.Duration
	usagePoll      bool

	pumpDone     chan struct{}
	eventsMu     sync.RWMutex
	eventsClosed bool
	closeOnce    sync.Once
	closeErr     error
}

var _ ports.ChatConversation = (*conversation)(nil)

// Asserted so a refactor cannot silently drop the deferred start: the controller
// feature-detects this interface, and without it the session/send would race
// the prompt_started notification (DESIGN 3.6).
var _ ports.ChatDeferredTurnStarter = (*conversation)(nil)
var _ ports.ChatInputResponder = (*conversation)(nil)
var _ ports.ChatProviderPreserver = (*conversation)(nil)
var _ ports.ChatProviderTerminator = (*conversation)(nil)
var _ ports.ChatLiveReconnector = (*conversation)(nil)
var _ ports.ChatModelLister = (*conversation)(nil)

func newConversation(proc *process, log *slog.Logger, runtimeHeaders runtimeHeadersResolver) *conversation {
	c := &conversation{
		proc:             proc,
		log:              log,
		events:           make(chan ports.ChatEvent, eventBuffer),
		pending:          make(map[string]*parkedRequest),
		byToolCallID:     make(map[string]string),
		recentlyResolved: make(map[string]any),
		turns:            newTurnTracker(),
		runtimeHeaders:   runtimeHeaders,
		approvalWait:     approvalWait,
		sendTimeout:      sendAckTimeout,
		usagePoll:        true,
		pumpDone:         make(chan struct{}),
		// A reconnected host replays unanswered server requests the moment the
		// read loop starts, so the reconcile gate must be up before newConnAt.
		reconciling: proc.reconnected,
	}
	c.conn = newConnAt(proc.stdin, proc.stdout, log, c.handleServerRequest, proc.nextRequestID)
	return c
}

// start records the open session and begins translating notifications. It is
// called once, after the session is open, so no event is emitted for a
// conversation the caller does not yet have a handle to.
func (c *conversation) start(sessionID string) {
	c.mu.Lock()
	c.sessionID = sessionID
	c.mu.Unlock()
	go c.pump()
}

// ProviderConversationID is the zcode session id AO persists for resume.
func (c *conversation) ProviderConversationID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// PreservesProviderOnClose tells the daemon controller that Close only detaches
// from a host; it must not project provider death or fail in-flight work.
func (c *conversation) PreservesProviderOnClose() bool { return c.proc.terminate != nil }

// ReconnectedLive distinguishes attachment to the same initialized process from
// native-history resume in a replacement process.
func (c *conversation) ReconnectedLive() bool { return c.proc.reconnected }

// Capabilities reports what this conversation can do.
func (c *conversation) Capabilities() ports.ChatCapabilities { return capabilities() }

// Events is the normalized stream. It closes when the conversation ends.
func (c *conversation) Events() <-chan ports.ChatEvent { return c.events }

// pump translates provider notifications into neutral events until the
// connection ends, then reports why and closes the stream.
func (c *conversation) pump() {
	defer close(c.pumpDone)
	defer func() {
		c.eventsMu.Lock()
		defer c.eventsMu.Unlock()
		c.eventsClosed = true
		close(c.events)
	}()

	c.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady})

	for n := range c.conn.notifs() {
		for _, ev := range normalizeNotification(n, c.turns, time.Now()) {
			switch ev.Kind {
			case ports.ChatEventApprovalResolved:
				if ev.RequestID == "" {
					ev.RequestID = c.requestIDForToolCall(resolvedToolCallID(n))
					ev.ProviderItemID = ev.RequestID
				}
				if ev.RequestID == "" {
					continue
				}
				// The provider resolved it (possibly elsewhere), so any card AO is
				// still showing is stale and the re-announce window is over.
				c.discardPending(ev.RequestID)
				c.forgetResolved(ev.RequestID)
			case ports.ChatEventInputResolved:
				c.discardPending(ev.RequestID)
				c.forgetResolved(ev.RequestID)
			case ports.ChatEventTurnCompleted:
				ev = c.settleInterrupt(ev)
				c.maybePollUsage(n, ev.ProviderTurnID)
			}
			c.emit(ev)
		}
	}

	// The connection ended. Say so explicitly rather than letting the stream go
	// quiet: a silent channel close is indistinguishable from an idle agent.
	state := ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerStopped}
	if err := c.conn.err(); err != nil {
		state.Err = err
	}
	c.emit(state)
	c.failPendingApprovals()
}

// settleInterrupt turns the failed settlement of a turn AO itself stopped into
// an interrupted one (U4: the exact settlement shape after session/stop is
// unobserved, so both cancelled and failed are accepted).
func (c *conversation) settleInterrupt(ev ports.ChatEvent) ports.ChatEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.interrupting == "" || c.interrupting != ev.ProviderTurnID {
		return ev
	}
	c.interrupting = ""
	if ev.TurnState == domain.TurnStateFailed {
		ev.TurnState = domain.TurnStateInterrupted
		ev.Err = nil
	}
	return ev
}

// maybePollUsage reads session/usage after a settlement that carried no inline
// usage (STATIC, U9). Best effort and off the pump goroutine so a slow or
// unsupported read never delays the next event.
func (c *conversation) maybePollUsage(n notification, turnID string) {
	if !c.usagePoll || inlineUsagePresent(n) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), usagePollTimeout)
		defer cancel()
		var result zcodeproto.SessionUsageResult
		if err := c.conn.request(ctx, zcodeproto.MethodSessionUsage,
			zcodeproto.SessionUsageParams{SessionID: c.ProviderConversationID()}, &result); err != nil {
			c.log.Debug("session/usage read failed", "error", err)
			return
		}
		if result.InputTokens == 0 && result.OutputTokens == 0 && result.TotalTokens == 0 {
			return
		}
		total := result.TotalTokens
		if total == 0 {
			total = result.InputTokens + result.OutputTokens
		}
		usage := &ports.ChatUsage{
			InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
			CachedTokens: result.CacheReadTokens, TotalTokens: total, TotalsKnown: true,
		}
		c.mu.Lock()
		if window := c.snapshot.Projection.ContextWindow; window > 0 {
			usage.ContextUsed = c.snapshot.Projection.ContextUsed
			usage.ContextWindow = window
			usage.ContextKnown = true
		}
		c.mu.Unlock()
		c.emit(ports.ChatEvent{Kind: ports.ChatEventUsage, ProviderTurnID: turnID, Usage: usage})
	}()
}

// inlineUsagePresent reports whether a turn.completed carried a usage object.
func inlineUsagePresent(n notification) bool {
	if n.Method != zcodeproto.MethodSessionEvent {
		return false
	}
	var p zcodeproto.SessionEventParams
	if err := json.Unmarshal(n.Params, &p); err != nil || p.Type != zcodeproto.EventTurnCompleted {
		return false
	}
	var payload zcodeproto.TurnCompletedPayload
	if err := json.Unmarshal(p.Payload, &payload); err != nil {
		return false
	}
	_, ok := usageFrom(payload.Usage)
	return ok
}

// resolvedToolCallID reads the toolCallId off a permission.resolved event.
func resolvedToolCallID(n notification) string {
	var p zcodeproto.SessionEventParams
	if err := json.Unmarshal(n.Params, &p); err != nil {
		return ""
	}
	var payload zcodeproto.PermissionResolvedPayload
	if err := json.Unmarshal(p.Payload, &payload); err != nil {
		return ""
	}
	return payload.ToolCallID
}

// emit delivers an event, preferring to drop a delta over blocking the reader. A
// lifecycle event is never dropped silently.
func (c *conversation) emit(ev ports.ChatEvent) {
	c.eventsMu.RLock()
	defer c.eventsMu.RUnlock()
	if c.eventsClosed {
		return
	}
	select {
	case c.events <- ev:
		return
	default:
	}

	switch ev.Kind {
	case ports.ChatEventMessageDelta, ports.ChatEventReasoningDelta:
		// The settled text arrives on turn.completed.response, so a dropped delta
		// costs smoothness, not correctness. Command output has no settled
		// restatement and is deliberately not droppable.
		c.log.Warn("dropped chat delta: consumer behind", "kind", ev.Kind, "item", ev.ProviderItemID)
		return
	}

	select {
	case c.events <- ev:
	case <-time.After(5 * time.Second):
		c.log.Error("dropped chat lifecycle event: consumer stalled", "kind", ev.Kind)
	}
}

// SendTurn prepares one message. Nothing is written to the provider: zcode's
// prompt_started notification precedes the session/send acknowledgement on the
// wire, so the RPC waits for StartDeferredTurn, after the controller has bound
// the minted id (DESIGN 3.6).
func (c *conversation) SendTurn(ctx context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	if err := ctx.Err(); err != nil {
		return ports.ChatTurnRef{}, err
	}
	if strings.TrimSpace(msg.Text) == "" {
		// There is no keystroke concept here: an empty message is a caller bug,
		// not a way to nudge the agent.
		return ports.ChatTurnRef{}, errors.New("chat message text is empty")
	}
	if msg.Settings.Model != "" {
		if _, ok := parseModelSelection(msg.Settings.Model); !ok {
			return ports.ChatTurnRef{}, fmt.Errorf("%w: model id %q is not providerId/modelId",
				ports.ErrChatConfigOptionInvalid, msg.Settings.Model)
		}
	}
	if msg.Settings.Approval != "" &&
		ports.NormalizePermissionMode(msg.Settings.Approval) != ports.NormalizePermissionMode(c.launchMode) {
		// session/send carries no mode. A per-turn change would need
		// session/setMode first (STATIC, DESIGN 4.4), which Phase 0 does not send:
		// every mode other than the launch mode is one the U5 guard refuses.
		return ports.ChatTurnRef{}, fmt.Errorf("%w: zcode turns run under the session's launch mode; requested %q",
			ports.ErrChatPermissionModeUnsupported, msg.Settings.Approval)
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ports.ChatTurnRef{}, errConversationClosed
	}
	if c.prepared != nil || c.turns.active() != "" {
		return ports.ChatTurnRef{}, errors.New("zcode conversation already has a turn in flight")
	}
	id := uuid.NewString()
	c.prepared = &preparedTurn{id: id, text: msg.Text, clientMessageID: msg.ClientMessageID, settings: msg.Settings}
	return ports.ChatTurnRef{ProviderTurnID: id}, nil
}

// DiscardDeferredTurn releases a prepared turn AO could not bind. It also
// disarms the tracker so a stray prompt_started cannot adopt the dropped id.
func (c *conversation) DiscardDeferredTurn(providerTurnID string) {
	c.mu.Lock()
	if c.prepared != nil && c.prepared.id == providerTurnID {
		c.prepared = nil
	}
	c.mu.Unlock()
	c.turns.cancel(providerTurnID)
}

// StartDeferredTurn writes the session/send for a prepared turn. The tracker is
// armed BEFORE the request is written because the server emits
// state.updated{reason:"prompt_started"} synchronously before it returns the
// {accepted} result; armed afterwards, that notification would reach the pump
// with no pending id and the real turn would never emit turn.started.
func (c *conversation) StartDeferredTurn(providerTurnID string) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errConversationClosed
	}
	if c.prepared == nil || c.prepared.id != providerTurnID {
		c.mu.Unlock()
		return fmt.Errorf("prepared turn %q not found", providerTurnID)
	}
	turn := *c.prepared
	c.prepared = nil
	content := turn.text
	spendPreamble := !c.preambleSpent && strings.TrimSpace(c.preamble) != ""
	if spendPreamble {
		// Standing instructions lead the first prompt that reaches the provider
		// (DESIGN 4.3): zcode has no system-prompt channel, and this is what the
		// TUI adapter's -p path does, so a -c restore sees the same first turn.
		content = strings.TrimSpace(c.preamble) + "\n\n" + turn.text
	}
	sessionID := c.sessionID
	c.mu.Unlock()

	params := zcodeproto.SessionSendParams{SessionID: sessionID, Content: content, InputID: turn.id}
	if turn.settings.Model != "" {
		if selection, ok := parseModelSelection(turn.settings.Model); ok {
			params.ModelSelection = &selection
		}
	}

	c.turns.prepare(turn.id)

	ctx, cancel := context.WithTimeout(context.Background(), c.sendTimeout)
	defer cancel()
	var result zcodeproto.SessionSendResult
	err := c.conn.request(ctx, zcodeproto.MethodSessionSend, params, &result)
	if err != nil && isSessionEvicted(err) {
		// The server evicted the resident session (DESIGN 3.7), not the process:
		// reload it once and retry the same send.
		if resumeErr := c.resumeEvicted(ctx); resumeErr == nil {
			err = c.conn.request(ctx, zcodeproto.MethodSessionSend, params, &result)
		}
	}
	if err == nil && !result.Accepted {
		err = errors.New("session/send was not accepted")
	}
	if err != nil {
		// Surfaced verbatim, with no auth interpretation: -32031 in particular is
		// not credentials-specific (zcodeproto/errors.go).
		if c.turns.cancel(turn.id) {
			c.emit(ports.ChatEvent{
				Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.id,
				TurnState: domain.TurnStateFailed, Err: ports.NewChatProviderFailure(err.Error(), "", nil),
			})
		}
		return fmt.Errorf("%s: %w%s", zcodeproto.MethodSessionSend, err, errorData(err))
	}
	if spendPreamble {
		c.mu.Lock()
		c.preambleSpent = true
		c.mu.Unlock()
	}
	return nil
}

// Interrupt stops the active turn with session/stop. The server returns {}
// even when idle, so the "nothing to stop" answer is AO's own.
func (c *conversation) Interrupt(ctx context.Context, providerTurnID string) error {
	active := c.turns.active()
	if providerTurnID == "" {
		providerTurnID = active
	}
	if providerTurnID == "" || providerTurnID != active {
		return ports.ErrChatNoActiveTurn
	}
	c.mu.Lock()
	c.interrupting = providerTurnID
	sessionID := c.sessionID
	c.mu.Unlock()
	if err := c.conn.request(ctx, zcodeproto.MethodSessionStop, zcodeproto.SessionStopParams{SessionID: sessionID}, nil); err != nil {
		c.mu.Lock()
		if c.interrupting == providerTurnID {
			c.interrupting = ""
		}
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", zcodeproto.MethodSessionStop, err)
	}
	return nil
}

// subscribe asks for session/event delivery (STATIC, U1 settled). A refusal is
// not fatal: the state.updated floor still settles turns.
func (c *conversation) subscribe(ctx context.Context, sessionID string) {
	var result zcodeproto.SessionSubscribeResult
	if err := c.conn.request(ctx, zcodeproto.MethodSessionSubscribe, zcodeproto.SessionSubscribeParams{
		SessionID: sessionID, DeliveryKind: deliveryKind,
	}, &result); err != nil {
		c.log.Warn("session/subscribe refused; running on the state.updated floor", "error", err)
	}
}

// readSnapshot refreshes the cached snapshot through session/read.
func (c *conversation) readSnapshot(ctx context.Context, messageLimit int) (zcodeproto.Snapshot, error) {
	var snapshot zcodeproto.Snapshot
	limit := messageLimit
	err := c.conn.request(ctx, zcodeproto.MethodSessionRead, zcodeproto.SessionReadParams{
		SessionID: c.ProviderConversationID(), MessageLimit: &limit,
	}, &snapshot)
	if err != nil {
		return zcodeproto.Snapshot{}, fmt.Errorf("%s: %w", zcodeproto.MethodSessionRead, err)
	}
	c.setSnapshot(snapshot)
	return snapshot, nil
}

// resumeEvicted reloads an evicted resident session in the same process.
func (c *conversation) resumeEvicted(ctx context.Context) error {
	c.mu.Lock()
	sessionID, workspace := c.sessionID, c.workspacePath
	c.mu.Unlock()
	var snapshot zcodeproto.Snapshot
	err := c.conn.request(ctx, zcodeproto.MethodSessionResume, zcodeproto.SessionResumeParams{
		SessionID: sessionID, Workspace: zcodeproto.Workspace{WorkspacePath: workspace, WorkspaceKey: workspace},
	}, &snapshot)
	if err != nil {
		c.log.Warn("session/resume after eviction failed", "error", err)
		return err
	}
	c.setSnapshot(snapshot)
	return nil
}

func (c *conversation) setSnapshot(snapshot zcodeproto.Snapshot) {
	c.mu.Lock()
	c.snapshot = snapshot
	c.mu.Unlock()
}

// resolveSessionID picks the session id from the PROVEN source (the
// runtime-preferences request raised inside session/create) and cross-checks
// the snapshot's session.sessionId (STATIC; never session.id).
func (c *conversation) resolveSessionID(snapshot zcodeproto.Snapshot) string {
	c.mu.Lock()
	fromRequest := c.preferencesSessionID
	c.mu.Unlock()
	fromSnapshot := snapshot.Session.SessionID
	switch {
	case fromRequest != "" && fromSnapshot != "" && fromRequest != fromSnapshot:
		c.log.Warn("session id mismatch between runtime-preferences request and snapshot",
			"request", fromRequest, "snapshot", fromSnapshot)
		return fromRequest
	case fromRequest != "":
		return fromRequest
	default:
		return fromSnapshot
	}
}

// Close releases the controller without touching provider-side history.
func (c *conversation) Close() error {
	c.closeOnce.Do(func() {
		if c.proc.terminate == nil {
			c.failPendingApprovals()
		}
		if c.proc.stop != nil {
			c.closeErr = c.proc.stop()
		}
	})
	return c.closeErr
}

// Terminate destroys the provider host. Close only detaches and is used by
// daemon-wide shutdown; explicit session/controller replacement calls this.
func (c *conversation) Terminate() error {
	c.closeOnce.Do(func() {
		c.failPendingApprovals()
		if c.proc.terminate != nil {
			c.closeErr = c.proc.terminate()
		} else if c.proc.stop != nil {
			c.closeErr = c.proc.stop()
		}
	})
	return c.closeErr
}

// isSessionEvicted recognises the -32004 that means the server dropped a
// resident session (message prefix "Session is not active:"), as distinct from
// the same code for an unknown id (DESIGN 3.7, U7).
func isSessionEvicted(err error) bool {
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		return false
	}
	return rpcErr.Code == zcodeproto.CodeSessionUnavailable &&
		strings.HasPrefix(rpcErr.Message, zcodeproto.SessionNotActivePrefix)
}

// isSessionNotFound recognises the -32004 for an id the server has no record
// of (message prefix "Session not found:"), which is what FreshIfMissing keys on.
func isSessionNotFound(err error) bool {
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		return false
	}
	return rpcErr.Code == zcodeproto.CodeSessionUnavailable &&
		strings.HasPrefix(rpcErr.Message, zcodeproto.SessionNotFoundPrefix)
}

// errorData renders a server error's data object for the surfaced message.
func errorData(err error) string {
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || len(rpcErr.Data) == 0 {
		return ""
	}
	return " (data: " + string(rpcErr.Data) + ")"
}
