package zcodeappserver

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/zcodeappserver/zcodeproto"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func notif(method, params string) notification {
	return notification{Method: method, Params: json.RawMessage(params)}
}

func sessionEvent(eventType, turnID, payload string) notification {
	return notif(zcodeproto.MethodSessionEvent,
		`{"type":"`+eventType+`","turnId":"`+turnID+`","sessionId":"sess-1","payload":`+payload+`}`)
}

func normalize(t *turnTracker, n notification) []ports.ChatEvent {
	return normalizeNotification(n, t, time.Unix(0, 0))
}

// armedTracker is a tracker with a prepared turn and the start observed, as it
// stands once session/send was accepted and prompt_started pumped.
func armedTracker(t *testing.T) *turnTracker {
	t.Helper()
	tracker := newTurnTracker()
	tracker.prepare("minted-1")
	if ev := tracker.observeStarted("t-native"); ev == nil || ev.ProviderTurnID != "minted-1" {
		t.Fatalf("observeStarted = %+v", ev)
	}
	return tracker
}

func TestReplayFixtureNotificationsNormalizeWithoutPanic(t *testing.T) {
	tracker := newTurnTracker()
	tracker.prepare("minted-1")
	var started int
	for _, line := range loadReplay(t, "proven-create-send.ndjson") {
		if line.Dir != "in" || line.Frame.ID != nil || line.Frame.Method == "" {
			continue
		}
		for _, ev := range normalize(tracker, notification{Method: line.Frame.Method, Params: line.Frame.Params}) {
			if ev.Kind == ports.ChatEventTurnStarted {
				started++
				if ev.ProviderTurnID != "minted-1" {
					t.Fatalf("turn.started id = %q", ev.ProviderTurnID)
				}
			}
		}
	}
	if started != 1 {
		t.Fatalf("turn.started count over the proven capture = %d, want 1 (prompt_started)", started)
	}
}

func TestNormalizeTable(t *testing.T) {
	cases := []struct {
		name  string
		n     notification
		kind  ports.ChatEventKind
		check func(t *testing.T, ev ports.ChatEvent)
	}{
		{
			name: "text delta via model.streaming",
			n:    sessionEvent("model.streaming", "t-native", `{"kind":"text_delta","delta":"He","assistantMessageId":"msg-1"}`),
			kind: ports.ChatEventMessageDelta,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.Delta != "He" || ev.ProviderItemID != "msg-1" || ev.ProviderTurnID != "minted-1" {
					t.Fatalf("message.delta = %+v", ev)
				}
			},
		},
		{
			name: "reasoning start",
			n:    sessionEvent("model.streaming", "t-native", `{"kind":"reasoning_start","partId":"p1"}`),
			kind: ports.ChatEventActivityStarted,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.ActivityKind != domain.ActivityKindReasoning || ev.ProviderItemID != "zcode-reasoning:p1" || ev.Summary != "Reasoning" {
					t.Fatalf("activity.started = %+v", ev)
				}
			},
		},
		{
			name: "reasoning delta",
			n:    sessionEvent("model.streaming", "t-native", `{"kind":"reasoning_delta","partId":"p1","delta":"hmm"}`),
			kind: ports.ChatEventReasoningDelta,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.Delta != "hmm" || ev.ProviderItemID != "zcode-reasoning:p1" {
					t.Fatalf("reasoning.delta = %+v", ev)
				}
			},
		},
		{
			name: "tool scheduled",
			n:    sessionEvent("tool.updated", "t-native", `{"kind":"scheduled","toolCallId":"tc1","toolName":"Bash","input":{"command":"date"}}`),
			kind: ports.ChatEventActivityStarted,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.ProviderItemID != "tc1" || ev.ActivityKind != domain.ActivityKindCommand || ev.ActivityStatus != domain.ActivityStatusRunning {
					t.Fatalf("activity.started = %+v", ev)
				}
				if ev.Summary != "Run date" || !strings.Contains(string(ev.Detail), `"command":"date"`) {
					t.Fatalf("summary/detail = %q / %s", ev.Summary, ev.Detail)
				}
			},
		},
		{
			name: "file tool is a file change",
			n:    sessionEvent("tool.updated", "t-native", `{"kind":"started","toolCallId":"tc2","toolName":"Edit","input":{"file_path":"a.go"}}`),
			kind: ports.ChatEventActivityStarted,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.ActivityKind != domain.ActivityKindFileChange {
					t.Fatalf("kind = %q", ev.ActivityKind)
				}
			},
		},
		{
			name: "mcp tool carries its server",
			n:    sessionEvent("tool.updated", "t-native", `{"kind":"started","toolCallId":"tc3","toolName":"mcp__ao__html_preview"}`),
			kind: ports.ChatEventActivityStarted,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.ActivityKind != domain.ActivityKindMCPTool || !strings.Contains(string(ev.Detail), `"server":"ao"`) {
					t.Fatalf("mcp activity = %+v", ev)
				}
			},
		},
		{
			name: "tool result completes the activity",
			n:    sessionEvent("tool.updated", "t-native", `{"kind":"result","toolCallId":"tc1","toolName":"Bash","result":{"exitCode":0},"duration":12}`),
			kind: ports.ChatEventActivityCompleted,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.ActivityStatus != domain.ActivityStatusCompleted || !strings.Contains(string(ev.Detail), `"durationMs":12`) {
					t.Fatalf("activity.completed = %+v", ev)
				}
			},
		},
		{
			name: "tool error fails the activity",
			n:    sessionEvent("tool.updated", "t-native", `{"kind":"error","toolCallId":"tc1","toolName":"Bash","error":{"message":"boom"}}`),
			kind: ports.ChatEventActivityCompleted,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.ActivityStatus != domain.ActivityStatusFailed || !strings.Contains(string(ev.Detail), "boom") {
					t.Fatalf("activity.completed = %+v", ev)
				}
			},
		},
		{
			name: "operation-event tool fallback",
			n:    notif(zcodeproto.MethodComputerUseOperationEvent, `{"kind":"tool-started","turnId":"t-native","toolCallId":"tc9","toolName":"Bash"}`),
			kind: ports.ChatEventActivityStarted,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.ProviderItemID != "tc9" || ev.ProviderTurnID != "minted-1" {
					t.Fatalf("activity.started = %+v", ev)
				}
			},
		},
		{
			name: "turn failed carries the provider prose with no auth cause",
			n:    sessionEvent("turn.failed", "t-native", `{"error":{"type":"AiSdkModelAdapterError","message":"model request failed","detail":"401 from upstream"},"turnPhase":"prepare"}`),
			kind: ports.ChatEventTurnCompleted,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.TurnState != domain.TurnStateFailed || ev.Err == nil || !strings.Contains(ev.Err.Error(), "model request failed") {
					t.Fatalf("turn.completed = %+v", ev)
				}
				if ev.NativeTurnID != "t-native" {
					t.Fatalf("native id = %q", ev.NativeTurnID)
				}
				if errors.Is(ev.Err, ports.ErrChatAuthRequired) {
					t.Fatal("turn.failed attached an auth cause without a proven native signal (U12)")
				}
			},
		},
		{
			name: "turn completed with usage",
			n:    sessionEvent("turn.completed", "t-native", `{"response":"OK","resultType":"success","usage":{"inputTokens":10,"outputTokens":2,"cacheReadTokens":1,"totalTokens":12}}`),
			kind: ports.ChatEventUsage,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.Usage == nil || ev.Usage.InputTokens != 10 || ev.Usage.CachedTokens != 1 || !ev.Usage.TotalsKnown || ev.Usage.ContextKnown {
					t.Fatalf("usage = %+v", ev.Usage)
				}
			},
		},
		{
			name: "cancelled result type is interrupted",
			n:    sessionEvent("turn.completed", "t-native", `{"resultType":"cancelled"}`),
			kind: ports.ChatEventTurnCompleted,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.TurnState != domain.TurnStateInterrupted {
					t.Fatalf("state = %q", ev.TurnState)
				}
			},
		},
		{
			name: "title updated",
			n:    sessionEvent("session.titleUpdated", "", `{"title":"Fix the build"}`),
			kind: ports.ChatEventThreadRenamed,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.Title != "Fix the build" {
					t.Fatalf("title = %q", ev.Title)
				}
			},
		},
		{
			name: "session closed is recorded, never torn down",
			n:    sessionEvent("session.closed", "", `{}`),
			kind: ports.ChatEventThreadState,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.ThreadState == nil || !ev.ThreadState.Closed || ev.ThreadState.Status != domain.ThreadStatusClosed {
					t.Fatalf("thread.state = %+v", ev.ThreadState)
				}
			},
		},
		{
			name: "operation-event session-closed",
			n:    notif(zcodeproto.MethodComputerUseOperationEvent, `{"kind":"session-closed","sessionId":"sess-1"}`),
			kind: ports.ChatEventThreadState,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.ThreadState == nil || !ev.ThreadState.Closed {
					t.Fatalf("thread.state = %+v", ev.ThreadState)
				}
			},
		},
		{
			name: "runtime headers cancelled is an error event",
			n:    notif(zcodeproto.MethodInteractionProviderRuntimeHeadersCancelled, `{"sessionId":"sess-1"}`),
			kind: ports.ChatEventError,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.Err == nil || !strings.Contains(ev.Err.Error(), "timed out") {
					t.Fatalf("error = %v", ev.Err)
				}
			},
		},
		{
			name: "user input resolved",
			n:    sessionEvent("userInput.resolved", "t-native", `{"requestId":"q1","action":"accept"}`),
			kind: ports.ChatEventInputResolved,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.RequestID != "q1" {
					t.Fatalf("input.resolved = %+v", ev)
				}
			},
		},
		{
			name: "permission resolved with requestId",
			n:    sessionEvent("permission.resolved", "t-native", `{"requestId":"r1","toolCallId":"tc1","decision":"allow"}`),
			kind: ports.ChatEventApprovalResolved,
			check: func(t *testing.T, ev ports.ChatEvent) {
				if ev.RequestID != "r1" || !strings.Contains(string(ev.Detail), `"toolCallId":"tc1"`) {
					t.Fatalf("approval.resolved = %+v", ev)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker := armedTracker(t)
			events := normalize(tracker, tc.n)
			var found *ports.ChatEvent
			for i := range events {
				if events[i].Kind == tc.kind {
					found = &events[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("no %q among %+v", tc.kind, events)
			}
			tc.check(t, *found)
		})
	}
}

func TestTextSourceLatchPreventsDoubleText(t *testing.T) {
	tracker := armedTracker(t)
	first := normalize(tracker, sessionEvent("model.streaming", "t-native", `{"kind":"text_delta","delta":"a","assistantMessageId":"m1"}`))
	if len(first) != 1 {
		t.Fatalf("model.streaming delta = %+v", first)
	}
	if got := normalize(tracker, sessionEvent("part.delta", "t-native", `{"messageId":"m1","partId":"p1","field":"text","delta":"a"}`)); len(got) != 0 {
		t.Fatalf("part.delta doubled the text: %+v", got)
	}
	completed := normalize(tracker, sessionEvent("turn.completed", "t-native", `{"response":"a","resultType":"success"}`))
	if len(completed) != 2 || completed[0].Kind != ports.ChatEventMessageCompleted || completed[0].ProviderItemID != "m1" || completed[1].Kind != ports.ChatEventTurnCompleted {
		t.Fatalf("turn.completed events = %+v", completed)
	}

	// A new turn latches afresh, and part.delta is honoured when it comes first.
	tracker.prepare("minted-2")
	normalize(tracker, notif(zcodeproto.MethodStateUpdated, `{"reason":"prompt_started"}`))
	if got := normalize(tracker, sessionEvent("part.delta", "", `{"messageId":"m2","field":"text","delta":"b"}`)); len(got) != 1 || got[0].ProviderItemID != "m2" || got[0].ProviderTurnID != "minted-2" {
		t.Fatalf("part.delta on a fresh turn = %+v", got)
	}
	if got := normalize(tracker, sessionEvent("model.streaming", "", `{"kind":"text_delta","delta":"b"}`)); len(got) != 0 {
		t.Fatalf("model.streaming doubled the text: %+v", got)
	}
}

func TestProgressTailSuffixDiff(t *testing.T) {
	tracker := armedTracker(t)
	progress := func(tail string) []ports.ChatEvent {
		return normalize(tracker, sessionEvent("tool.updated", "t-native", `{"kind":"progress","toolCallId":"tc1","stdoutTail":`+strconvQuote(tail)+`}`))
	}
	if got := progress("abc"); len(got) != 1 || got[0].Delta != "abc" || got[0].Kind != ports.ChatEventCommandOutputDelta {
		t.Fatalf("first progress = %+v", got)
	}
	if got := progress("abcdef"); len(got) != 1 || got[0].Delta != "def" {
		t.Fatalf("extension = %+v", got)
	}
	if got := progress("abcdef"); len(got) != 0 {
		t.Fatalf("unchanged tail emitted: %+v", got)
	}
	if got := progress("zzz"); len(got) != 0 {
		t.Fatalf("non-extending tail emitted (Phase 0 suffix-diff rule): %+v", got)
	}

	// stdout and stderr are diffed independently: concatenated, a growing
	// stdout would shift stderr and no later tail would extend the last one.
	both := func(stdout, stderr string) []string {
		var deltas []string
		for _, ev := range normalize(tracker, sessionEvent("tool.updated", "t-native",
			`{"kind":"progress","toolCallId":"tc2","stdoutTail":`+strconvQuote(stdout)+`,"stderrTail":`+strconvQuote(stderr)+`}`)) {
			if ev.Kind != ports.ChatEventCommandOutputDelta || ev.ProviderItemID != "tc2" {
				t.Fatalf("progress event = %+v", ev)
			}
			deltas = append(deltas, ev.Delta)
		}
		return deltas
	}
	for _, step := range []struct {
		stdout, stderr string
		want           []string
	}{
		{"ab", "e", []string{"ab", "e"}},
		{"abc", "e", []string{"c"}},
		{"abc", "ef", []string{"f"}},
		{"abcd", "efg", []string{"d", "g"}},
		{"abcd", "efg", nil},
	} {
		if got := both(step.stdout, step.stderr); !reflect.DeepEqual(got, step.want) {
			t.Fatalf("progress(%q, %q) = %v, want %v", step.stdout, step.stderr, got, step.want)
		}
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestNormalizeToleratesMalformedParams(t *testing.T) {
	methods := []string{
		zcodeproto.MethodStateUpdated,
		zcodeproto.MethodComputerUseOperationEvent,
		zcodeproto.MethodSessionEvent,
		zcodeproto.MethodInteractionProviderRuntimeHeadersCancelled,
	}
	for _, method := range methods {
		for _, params := range []string{`"not an object"`, `42`, `[1,2]`, `null`, ``, `{"type":"turn.completed","payload":"nope"}`, `{"type":"tool.updated","payload":[]}`, `{"kind":7}`} {
			tracker := armedTracker(t)
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s with params %s panicked: %v", method, params, r)
					}
				}()
				_ = normalize(tracker, notif(method, params))
			}()
		}
	}
	// The runtime-headers cancellation is the one method whose params carry
	// nothing the event needs; everything else yields nil on garbage.
	tracker := armedTracker(t)
	for _, method := range methods[:3] {
		if got := normalize(tracker, notif(method, `"not an object"`)); got != nil {
			t.Fatalf("%s with garbage params emitted %+v", method, got)
		}
	}
}

func TestNoiseIsDropped(t *testing.T) {
	tracker := armedTracker(t)
	noise := []struct {
		tag string
		n   notification
	}{
		{"PROVEN probe:39", notif(zcodeproto.MethodStartupStorageState, `{"phase":"waiting_for_lock"}`)},
		{"PROVEN probe:39", notif(zcodeproto.MethodProcessResourceSample, `{"rss":1}`)},
		{"PROVEN probe:39", notif(zcodeproto.MethodProcessMcpTelemetry, `{}`)},
		{"STATIC sT", notif(zcodeproto.MethodProcessMcpResourceSamples, `{}`)},
		{"STATIC sT", notif(zcodeproto.MethodProcessToolExecResource, `{}`)},
		{"STATIC sT", notif(zcodeproto.MethodProcessNetworkRequests, `{}`)},
		{"STATIC sT", notif(zcodeproto.MethodPluginsOperationProgress, `{}`)},
		{"STATIC Sg", notif("v4/desktop/anything", `{}`)},
		{"STATIC zXt", sessionEvent("session.updated", "t-native", `{}`)},
		{"STATIC zXt", sessionEvent("checkpoint.created", "t-native", `{}`)},
		{"STATIC zXt", sessionEvent("rewind.triggered", "t-native", `{}`)},
		{"STATIC zXt", sessionEvent("streamRecovery.updated", "t-native", `{}`)},
		{"STATIC zXt", sessionEvent("turn.steerQueued", "t-native", `{}`)},
		{"STATIC zXt", sessionEvent("turn.steerDrained", "t-native", `{}`)},
		{"STATIC zXt", sessionEvent("message.created", "t-native", `{}`)},
		{"STATIC zXt", sessionEvent("part.started", "t-native", `{}`)},
		{"STATIC zXt", sessionEvent("part.upserted", "t-native", `{}`)},
		{"STATIC zXt", sessionEvent("part.removed", "t-native", `{}`)},
		{"STATIC zXt", sessionEvent("permission.requested", "t-native", `{"toolCallId":"tc1"}`)},
		{"STATIC Lhc/Xmc", notif(zcodeproto.MethodStateUpdated, `{"reason":"model_changed","patch":{}}`)},
		{"STATIC Lhc/Xmc", notif(zcodeproto.MethodStateUpdated, `{"reason":"settings_updated"}`)},
		{"STATIC Oai", notif(zcodeproto.MethodComputerUseOperationEvent, `{"kind":"heartbeat"}`)},
	}
	for _, entry := range noise {
		if got := normalize(tracker, entry.n); len(got) != 0 {
			t.Fatalf("%s (%s) emitted %+v", entry.n.Method, entry.tag, got)
		}
	}
	if tracker.currentTurn() != "minted-1" {
		t.Fatal("noise settled the turn")
	}
}

func TestUnknownResultTypeFailsClosed(t *testing.T) {
	for _, resultType := range []string{"", "error_timeout", "something_new"} {
		tracker := armedTracker(t)
		events := normalize(tracker, sessionEvent("turn.completed", "t-native", `{"resultType":`+strconvQuote(resultType)+`}`))
		if len(events) != 1 || events[0].Kind != ports.ChatEventTurnCompleted {
			t.Fatalf("%q: events = %+v", resultType, events)
		}
		if events[0].TurnState != domain.TurnStateFailed || events[0].Err == nil {
			t.Fatalf("%q settled as %q (err %v), want failed with an error", resultType, events[0].TurnState, events[0].Err)
		}
	}
}

func TestTurnLifecycleDedupesAcrossThreeSignals(t *testing.T) {
	signals := map[string]notification{
		"prompt_started": notif(zcodeproto.MethodStateUpdated, `{"reason":"prompt_started"}`),
		"operation":      notif(zcodeproto.MethodComputerUseOperationEvent, `{"kind":"turn-started","turnId":"t-native"}`),
		"session/event":  sessionEvent("turn.started", "t-native", `{"inputId":"minted-1","turnNumber":1}`),
	}
	orders := [][]string{
		{"prompt_started", "operation", "session/event"},
		{"prompt_started", "session/event", "operation"},
		{"operation", "prompt_started", "session/event"},
		{"operation", "session/event", "prompt_started"},
		{"session/event", "prompt_started", "operation"},
		{"session/event", "operation", "prompt_started"},
	}
	for _, order := range orders {
		t.Run(strings.Join(order, ">"), func(t *testing.T) {
			tracker := newTurnTracker()
			tracker.prepare("minted-1")
			var started []ports.ChatEvent
			for _, name := range order {
				for _, ev := range normalize(tracker, signals[name]) {
					if ev.Kind == ports.ChatEventTurnStarted {
						started = append(started, ev)
					}
				}
			}
			if len(started) != 1 || started[0].ProviderTurnID != "minted-1" {
				t.Fatalf("turn.started = %+v", started)
			}
			if order[0] != "prompt_started" && started[0].NativeTurnID != "t-native" {
				t.Fatalf("native id missing on a native-carrying first signal: %+v", started[0])
			}
			if tracker.nativeFor("minted-1") != "t-native" || tracker.mintedFor("t-native") != "minted-1" {
				t.Fatal("native mapping not learned")
			}
			completed := normalize(tracker, notif(zcodeproto.MethodStateUpdated, `{"reason":"prompt_completed"}`))
			if len(completed) != 1 || completed[0].NativeTurnID != "t-native" || completed[0].TurnState != domain.TurnStateCompleted {
				t.Fatalf("turn.completed = %+v", completed)
			}
			if got := normalize(tracker, notif(zcodeproto.MethodComputerUseOperationEvent, `{"kind":"turn-completed","turnId":"t-native"}`)); len(got) != 0 {
				t.Fatalf("settled twice: %+v", got)
			}
		})
	}
}

func TestProviderInitiatedTurnGetsANamespacedID(t *testing.T) {
	tracker := newTurnTracker()
	events := normalize(tracker, notif(zcodeproto.MethodComputerUseOperationEvent, `{"kind":"turn-started","turnId":"t-goal"}`))
	if len(events) != 1 || events[0].ProviderTurnID != providerTurnPrefix+"t-goal" {
		t.Fatalf("provider-initiated turn = %+v", events)
	}
	// A bare prompt_started with nothing prepared names nothing: dropped.
	if got := normalize(newTurnTracker(), notif(zcodeproto.MethodStateUpdated, `{"reason":"prompt_started"}`)); len(got) != 0 {
		t.Fatalf("uncorrelated prompt_started emitted %+v", got)
	}
}
