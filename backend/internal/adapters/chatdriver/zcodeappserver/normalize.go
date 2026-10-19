package zcodeappserver

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/zcodeappserver/zcodeproto"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// normalizeNotification translates one provider notification into AO events
// per the table in DESIGN 4.2. The tracker supplies the turn correlation; the
// clock is passed in so the function stays deterministic under test.
//
// Malformed params never panic: every case decodes into its zcodeproto struct
// and returns nil on error. Anything the table does not list is dropped —
// inventing semantics for a frame AO does not understand is worse than silence.
func normalizeNotification(n notification, t *turnTracker, _ time.Time) []ports.ChatEvent {
	switch n.Method {
	case zcodeproto.MethodStateUpdated:
		return normalizeStateUpdated(n.Params, t)
	case zcodeproto.MethodComputerUseOperationEvent:
		return normalizeOperationEvent(n.Params, t)
	case zcodeproto.MethodSessionEvent:
		return normalizeSessionEvent(n.Params, t)
	case zcodeproto.MethodInteractionProviderRuntimeHeadersCancelled:
		// The server gave up waiting for credentials AO did not supply (U0). The
		// turn settles separately; this only explains why.
		return []ports.ChatEvent{{
			Kind:           ports.ChatEventError,
			ProviderTurnID: t.currentTurn(),
			Err:            ports.NewChatProviderFailure("Provider credentials request timed out", "", nil),
		}}
	}
	return nil
}

// normalizeStateUpdated handles the PROVEN progress signal. prompt_started is
// on the wire before the session/send acknowledgement, which is why the tracker
// is armed before the request is written (DESIGN 3.6).
func normalizeStateUpdated(params json.RawMessage, t *turnTracker) []ports.ChatEvent {
	var p zcodeproto.StateUpdatedParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil
	}
	switch p.Reason {
	case zcodeproto.ReasonPromptStarted:
		return one(t.observeStarted(""))
	case zcodeproto.ReasonPromptCompleted:
		return one(t.observeCompleted(domain.TurnStateCompleted, nil))
	case zcodeproto.ReasonPromptFailed:
		return one(t.observeCompleted(domain.TurnStateFailed,
			ports.NewChatProviderFailure("Turn failed", "", nil)))
	}
	return nil
}

// normalizeOperationEvent handles computer-use/operation-event, which arrives
// whether or not the session is subscribed and carries the native turn id.
func normalizeOperationEvent(params json.RawMessage, t *turnTracker) []ports.ChatEvent {
	var p zcodeproto.OperationEventParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil
	}
	switch p.Kind {
	case zcodeproto.OperationTurnStarted:
		return one(t.observeStarted(p.TurnID))
	case zcodeproto.OperationTurnCompleted:
		return one(t.observeCompleted(domain.TurnStateCompleted, nil))
	case zcodeproto.OperationTurnFailed:
		return one(t.observeCompleted(domain.TurnStateFailed,
			ports.NewChatProviderFailure("Turn failed", "", nil)))
	case zcodeproto.OperationToolScheduled, zcodeproto.OperationToolStarted:
		if p.ToolCallID == "" {
			return nil
		}
		// Fallback for an unsubscribed session: no completion is available, so
		// the turn settlement path settles still-running activities.
		return []ports.ChatEvent{{
			Kind:           ports.ChatEventActivityStarted,
			ProviderTurnID: turnFor(t, p.TurnID),
			ProviderItemID: p.ToolCallID,
			ActivityKind:   kindForTool(p.ToolName),
			ActivityStatus: domain.ActivityStatusRunning,
			Summary:        toolSummary(p.ToolName, nil),
			Detail:         toolDetail(p.ToolName, nil, nil),
		}}
	case zcodeproto.OperationSessionClosed:
		return []ports.ChatEvent{closedThreadState()}
	}
	return nil
}

// normalizeSessionEvent handles session/event, the subscribed stream that
// carries assistant text, tool lifecycle and the authoritative settlement.
func normalizeSessionEvent(params json.RawMessage, t *turnTracker) []ports.ChatEvent {
	var p zcodeproto.SessionEventParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil
	}
	switch p.Type {
	case zcodeproto.EventTurnStarted:
		var payload zcodeproto.TurnStartedPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil && len(p.Payload) > 0 {
			return nil
		}
		// TODO(U3): payload.inputId is expected to echo the inputId sent on
		// session/send, i.e. the minted id the tracker is already armed with.
		// Until P1 proves it, the echo is not used for correlation.
		return one(t.observeStarted(p.TurnID))

	case zcodeproto.EventModelStreaming:
		var payload zcodeproto.ModelStreamingPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			return nil
		}
		turn := turnFor(t, p.TurnID)
		switch payload.Kind {
		case zcodeproto.StreamingTextDelta:
			if !t.latchTextSource(zcodeproto.EventModelStreaming) {
				return nil
			}
			return []ports.ChatEvent{{
				Kind:           ports.ChatEventMessageDelta,
				ProviderTurnID: turn,
				ProviderItemID: t.replyItem(payload.AssistantMessageID, turn),
				Delta:          payload.Delta,
			}}
		case zcodeproto.StreamingReasoningStart:
			return []ports.ChatEvent{{
				Kind:           ports.ChatEventActivityStarted,
				ProviderTurnID: turn,
				ProviderItemID: reasoningItem(payload.PartID, turn),
				ActivityKind:   domain.ActivityKindReasoning,
				ActivityStatus: domain.ActivityStatusRunning,
				Summary:        "Reasoning",
			}}
		case zcodeproto.StreamingReasoningDelta:
			return []ports.ChatEvent{{
				Kind:           ports.ChatEventReasoningDelta,
				ProviderTurnID: turn,
				ProviderItemID: reasoningItem(payload.PartID, turn),
				Delta:          payload.Delta,
			}}
		}
		return nil

	case zcodeproto.EventPartDelta:
		var payload zcodeproto.PartDeltaPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			return nil
		}
		if payload.Field != "text" {
			return nil
		}
		// Honoured only if model.streaming text_delta was not seen this turn, so
		// a build that emits both does not double the text (U2 decides which is
		// primary).
		if !t.latchTextSource(zcodeproto.EventPartDelta) {
			return nil
		}
		turn := turnFor(t, p.TurnID)
		return []ports.ChatEvent{{
			Kind:           ports.ChatEventMessageDelta,
			ProviderTurnID: turn,
			ProviderItemID: t.replyItem(payload.MessageID, turn),
			Delta:          payload.Delta,
		}}

	case zcodeproto.EventTurnCompleted:
		var payload zcodeproto.TurnCompletedPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			return nil
		}
		turn := turnFor(t, p.TurnID)
		var events []ports.ChatEvent
		if payload.Response != "" || t.replyItemKnown() {
			events = append(events, ports.ChatEvent{
				Kind:           ports.ChatEventMessageCompleted,
				ProviderTurnID: turn,
				ProviderItemID: t.replyItem("", turn),
				Text:           payload.Response,
			})
		}
		if usage, ok := usageFrom(payload.Usage); ok {
			events = append(events, ports.ChatEvent{Kind: ports.ChatEventUsage, ProviderTurnID: turn, Usage: usage})
		}
		state, failure := turnStateFromResultType(payload.ResultType)
		return append(events, one(t.observeCompleted(state, failure))...)

	case zcodeproto.EventTurnFailed:
		var payload zcodeproto.TurnFailedPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			return nil
		}
		// TODO(U12): attach ports.ErrChatAuthRequired as the cause only once P10
		// records the native shape of an expired-token failure. Until then the
		// provider's prose is surfaced with no auth interpretation.
		return one(t.observeCompleted(domain.TurnStateFailed,
			ports.NewChatProviderFailure(payload.Error.Message, payload.Error.Detail, nil)))

	case zcodeproto.EventToolUpdated:
		var payload zcodeproto.ToolUpdatedPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			return nil
		}
		return normalizeToolUpdated(payload, turnFor(t, p.TurnID), t)

	case zcodeproto.EventPermissionResolved:
		var payload zcodeproto.PermissionResolvedPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			return nil
		}
		if payload.RequestID == "" && payload.ToolCallID == "" {
			return nil
		}
		// RequestID may be empty: `Uhr` makes requestId optional and toolCallId
		// required. The conversation pump fills it from its toolCallId index so
		// the parked request is cleared either way (DESIGN 4.2).
		detail, _ := json.Marshal(map[string]any{
			"toolCallId": payload.ToolCallID, "toolName": payload.ToolName,
			"decision": payload.Decision, "reason": payload.Reason,
		})
		return []ports.ChatEvent{{
			Kind:           ports.ChatEventApprovalResolved,
			ProviderTurnID: turnFor(t, p.TurnID),
			ProviderItemID: payload.RequestID,
			RequestID:      payload.RequestID,
			Detail:         detail,
		}}

	case zcodeproto.EventUserInputResolved:
		var payload zcodeproto.UserInputResolvedPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil || payload.RequestID == "" {
			return nil
		}
		return []ports.ChatEvent{{
			Kind:           ports.ChatEventInputResolved,
			ProviderTurnID: turnFor(t, p.TurnID),
			ProviderItemID: payload.RequestID,
			RequestID:      payload.RequestID,
		}}

	case zcodeproto.EventSessionTitleUpdated:
		var payload zcodeproto.TitleUpdatedPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			return nil
		}
		return []ports.ChatEvent{{Kind: ports.ChatEventThreadRenamed, Title: payload.Title}}

	case zcodeproto.EventSessionClosed:
		return []ports.ChatEvent{closedThreadState()}
	}
	return nil
}

// normalizeToolUpdated maps the kind-discriminated tool.updated payload onto
// activity lifecycle events on one item id (the toolCallId).
func normalizeToolUpdated(payload zcodeproto.ToolUpdatedPayload, turn string, t *turnTracker) []ports.ChatEvent {
	if payload.ToolCallID == "" {
		return nil
	}
	switch payload.Kind {
	case zcodeproto.ToolScheduled, zcodeproto.ToolStarted:
		return []ports.ChatEvent{{
			Kind:           ports.ChatEventActivityStarted,
			ProviderTurnID: turn,
			ProviderItemID: payload.ToolCallID,
			ActivityKind:   kindForTool(payload.ToolName),
			ActivityStatus: domain.ActivityStatusRunning,
			Summary:        toolSummary(payload.ToolName, payload.Input),
			Detail:         toolDetail(payload.ToolName, payload.Input, nil),
		}}
	case zcodeproto.ToolProgress:
		stdout := payload.StdoutTail
		if stdout == "" {
			stdout = payload.OutputPreview
		}
		// Whether the tails are cumulative or incremental is unknown; Phase 0
		// emits only the extension beyond the previous tail (DESIGN 4.2). Each
		// stream is diffed against its own memory: concatenated, a growing
		// stdout would shift stderr and no later tail would extend the last.
		var events []ports.ChatEvent
		for _, stream := range []struct{ key, tail string }{
			{payload.ToolCallID + "\x00out", stdout},
			{payload.ToolCallID + "\x00err", payload.StderrTail},
		} {
			if delta := t.progressDelta(stream.key, stream.tail); delta != "" {
				events = append(events, ports.ChatEvent{
					Kind:           ports.ChatEventCommandOutputDelta,
					ProviderTurnID: turn,
					ProviderItemID: payload.ToolCallID,
					Delta:          delta,
				})
			}
		}
		return events
	case zcodeproto.ToolResult:
		extra := map[string]any{}
		if len(payload.Result) > 0 {
			extra["result"] = json.RawMessage(payload.Result)
		}
		if payload.Duration > 0 {
			extra["durationMs"] = payload.Duration
		}
		return []ports.ChatEvent{{
			Kind:           ports.ChatEventActivityCompleted,
			ProviderTurnID: turn,
			ProviderItemID: payload.ToolCallID,
			ActivityKind:   kindForTool(payload.ToolName),
			ActivityStatus: domain.ActivityStatusCompleted,
			Summary:        toolSummary(payload.ToolName, payload.Input),
			Detail:         toolDetail(payload.ToolName, payload.Input, extra),
		}}
	case zcodeproto.ToolError:
		extra := map[string]any{}
		if len(payload.Error) > 0 {
			extra["error"] = json.RawMessage(payload.Error)
		}
		return []ports.ChatEvent{{
			Kind:           ports.ChatEventActivityCompleted,
			ProviderTurnID: turn,
			ProviderItemID: payload.ToolCallID,
			ActivityKind:   kindForTool(payload.ToolName),
			ActivityStatus: domain.ActivityStatusFailed,
			Summary:        toolSummary(payload.ToolName, payload.Input),
			Detail:         toolDetail(payload.ToolName, payload.Input, extra),
		}}
	}
	return nil
}

// kindForTool maps a zcode tool name onto AO's activity kind (DESIGN 4.2).
func kindForTool(toolName string) domain.ActivityKind {
	switch {
	case toolName == "Write", toolName == "Edit", toolName == "MultiEdit", toolName == "NotebookEdit":
		return domain.ActivityKindFileChange
	case strings.HasPrefix(toolName, "mcp__"):
		return domain.ActivityKindMCPTool
	default:
		return domain.ActivityKindCommand
	}
}

// toolSummary is the one-line label for a tool activity.
func toolSummary(toolName string, input json.RawMessage) string {
	if command := inputCommand(input); command != "" {
		return "Run " + command
	}
	if toolName == "" {
		return "Tool call"
	}
	return toolName
}

// toolDetail builds the neutral detail payload using codex's key vocabulary
// (command, cwd, toolName, server, arguments, result, error, durationMs) because
// the UI renders those keys.
func toolDetail(toolName string, input json.RawMessage, extra map[string]any) []byte {
	detail := map[string]any{}
	if toolName != "" {
		detail["toolName"] = toolName
	}
	if strings.HasPrefix(toolName, "mcp__") {
		parts := strings.SplitN(toolName, "__", 3)
		if len(parts) >= 2 {
			detail["server"] = parts[1]
		}
	}
	if command := inputCommand(input); command != "" {
		detail["command"] = command
	}
	if len(input) > 0 && json.Valid(input) {
		detail["arguments"] = json.RawMessage(input)
	}
	for key, value := range extra {
		detail[key] = value
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return nil
	}
	return encoded
}

// inputCommand reads a `command` string off a tool input object when present.
func inputCommand(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var object struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &object); err != nil {
		return ""
	}
	return object.Command
}

// turnStateFromResultType maps turn.completed.resultType onto AO's turn state.
// Unknown (including empty) is failed rather than completed, like codex's
// turnStateFrom: claiming success AO did not observe is worse than reporting
// an honest unknown failure.
func turnStateFromResultType(resultType string) (domain.TurnState, error) {
	switch resultType {
	case "success":
		return domain.TurnStateCompleted, nil
	case "cancelled", "canceled", "interrupted":
		return domain.TurnStateInterrupted, nil
	case "":
		return domain.TurnStateFailed, ports.NewChatProviderFailure("Turn ended without a result type", "", nil)
	default:
		return domain.TurnStateFailed, ports.NewChatProviderFailure(resultType, "", nil)
	}
}

// usageFrom decodes turn.completed.usage into AO's accounting (U9). Context
// occupancy is never derived from totals (ports.ChatUsage rule).
func usageFrom(raw json.RawMessage) (*ports.ChatUsage, bool) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, false
	}
	var usage zcodeproto.TurnUsage
	if err := json.Unmarshal(raw, &usage); err != nil {
		return nil, false
	}
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.TotalTokens == 0 {
		return nil, false
	}
	total := usage.TotalTokens
	if total == 0 {
		total = usage.InputTokens + usage.OutputTokens
	}
	return &ports.ChatUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		CachedTokens: usage.CacheReadTokens,
		TotalTokens:  total,
		TotalsKnown:  true,
	}, true
}

func closedThreadState() ports.ChatEvent {
	return ports.ChatEvent{
		Kind:        ports.ChatEventThreadState,
		ThreadState: &ports.ChatThreadState{Status: domain.ThreadStatusClosed, Closed: true},
	}
}

func reasoningItem(partID, turn string) string {
	if partID != "" {
		return "zcode-reasoning:" + partID
	}
	return "zcode-reasoning:" + turn
}

// turnFor resolves a native turn id to AO's id, falling back to the turn in
// flight: the open one, or the armed one whose start signal the pump has not
// processed yet. Server requests are answered on their own goroutines ahead of
// the pump, so an approval raised right after session/send would otherwise be
// filed under no turn (DESIGN 3.5).
func turnFor(t *turnTracker, native string) string {
	if native != "" {
		if minted := t.mintedFor(native); minted != "" {
			return minted
		}
	}
	return t.active()
}

func one(ev *ports.ChatEvent) []ports.ChatEvent {
	if ev == nil {
		return nil
	}
	return []ports.ChatEvent{*ev}
}

// rawID stringifies a JSON-RPC id, which may be a number or a string. `null`,
// absent and empty all yield "".
func rawID(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	return strings.Trim(s, `"`)
}
