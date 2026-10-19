package zcodeappserver

import (
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// providerTurnPrefix namespaces the minted id of a turn the provider started on
// its own (goal continuation, `nhc`), so the controller can adopt it without
// ever confusing it with the turn AO prepared.
const providerTurnPrefix = "zcode-turn:"

// turnTracker is the one piece of per-conversation state the normalizer needs
// (DESIGN 4.1). zcode names a turn in three places that arrive in a fixed order
// for one session/send:
//
//  1. state.updated{reason:"prompt_started"} — emitted BEFORE the {accepted}
//     result, no turn id;
//  2. computer-use/operation-event{kind:"turn-started", turnId};
//  3. session/event turn.started{turnId, payload.inputId} — only when subscribed.
//
// AO's controller needs one ProviderTurnID that SendTurn returned before any of
// them could be observed, so the tracker is armed with the minted id by
// prepare() before the session/send frame is written, emits turn.started once on
// the first of the three signals, and fills in the native id as it learns it.
// Completion is likewise emitted once, preferring session/event turn.completed
// or turn.failed over the state.updated prompt_* fallback, which is always last.
type turnTracker struct {
	mu sync.Mutex

	// pending is the minted id armed by prepare and not yet observed started.
	pending string
	// current is the open turn's AO id; native is its provider id when known.
	current string
	native  string
	open    bool
	// startedOut is set once the pump has put the open turn's turn.started on
	// the event stream, and failAfterStart holds the failed completion a cancel
	// left for the pump to emit right after it (the two must not be reordered).
	startedOut     bool
	failAfterStart *ports.ChatEvent

	// textSource latches which delta kind carried assistant text this turn so
	// model.streaming and part.delta never double the text (DESIGN 4.2).
	textSource string
	// replyItemID is the item id the turn's assistant text was streamed under,
	// so message.completed settles the same row the deltas built.
	replyItemID string
	// toolTails remembers the last progress tail per tool call for the suffix
	// diff of DESIGN 4.2.
	toolTails map[string]string

	// Mappings outlive the turn so Interrupt and fork anchors can translate
	// after settlement. Bounded by the number of turns in one process.
	nativeByMinted map[string]string
	mintedByNative map[string]string
}

func newTurnTracker() *turnTracker {
	return &turnTracker{
		nativeByMinted: make(map[string]string),
		mintedByNative: make(map[string]string),
		toolTails:      make(map[string]string),
	}
}

// prepare arms the tracker with the id SendTurn minted. It must run before the
// session/send request is written (DESIGN 3.6).
func (t *turnTracker) prepare(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending = id
}

// disarm drops a prepared id whose session/send was never written.
func (t *turnTracker) disarm(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == id {
		t.pending = ""
	}
}

// cancel disarms a prepared id after the session/send request failed. When the
// turn was already observed started, a failed turn.completed must reach the
// stream so the controller cannot stay Busy — and it must follow the pump's
// turn.started, which the pump emits on its own goroutine after observeStarted
// returns. So the caller emits the failure itself only when the pump reports
// turn.started already out (emitNow); otherwise the failure is parked here and
// the pump emits it right after turn.started (markStartedEmitted).
func (t *turnTracker) cancel(id string, failure ports.ChatEvent) (emitNow bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == id {
		t.pending = ""
	}
	if !t.open || t.current != id {
		return false
	}
	startedOut := t.startedOut
	t.closeLocked()
	if startedOut {
		return true
	}
	t.failAfterStart = &failure
	return false
}

// markStartedEmitted records that the pump put turn.started for id on the
// stream and hands back the failed completion a cancel parked for it, if any.
func (t *turnTracker) markStartedEmitted(id string) *ports.ChatEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open && t.current == id {
		t.startedOut = true
	}
	if t.failAfterStart != nil && t.failAfterStart.ProviderTurnID == id {
		failure := t.failAfterStart
		t.failAfterStart = nil
		return failure
	}
	return nil
}

// adopt opens the turn a previous daemon started (live reconnect): minted is
// the durable id the controller restored, native the app-server's id for it
// when session/read reported one. Nothing is emitted — the controller already
// owns the turn — but every later signal for it now correlates, so a completion
// replayed from the detach window settles it instead of being dropped.
func (t *turnTracker) adopt(minted, native string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending = ""
	t.openLocked(minted, native)
	t.startedOut = true
}

// openLocked opens a turn under the lock, resetting the per-turn latches.
func (t *turnTracker) openLocked(id, native string) {
	t.open = true
	t.current = id
	t.native = native
	t.textSource = ""
	t.replyItemID = ""
	t.startedOut = false
	t.toolTails = make(map[string]string)
	if native != "" {
		t.nativeByMinted[id] = native
		t.mintedByNative[native] = id
	}
}

// closeLocked settles the open turn under the lock.
func (t *turnTracker) closeLocked() {
	t.open = false
	t.current = ""
	t.native = ""
	t.textSource = ""
	t.replyItemID = ""
	t.startedOut = false
}

// observeStarted folds one of the three start signals. It returns the
// turn.started event exactly once per turn and nil for every later signal, which
// only fills in the native id.
func (t *turnTracker) observeStarted(native string) *ports.ChatEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open {
		if t.native == "" && native != "" {
			t.native = native
			t.nativeByMinted[t.current] = native
			t.mintedByNative[native] = t.current
		}
		return nil
	}
	id := t.pending
	switch {
	case id != "":
		t.pending = ""
	case native != "":
		id = providerTurnPrefix + native
	default:
		// A start signal with neither a prepared turn nor a native id names
		// nothing AO can correlate; dropping it is safer than inventing a turn.
		return nil
	}
	t.openLocked(id, native)
	return &ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: id, NativeTurnID: native}
}

// observeCompleted settles the open turn. It returns the turn.completed event
// once and nil when nothing is open, so the prompt_* fallback cannot fire after
// session/event already settled the turn.
func (t *turnTracker) observeCompleted(state domain.TurnState, err error) *ports.ChatEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.open {
		return nil
	}
	ev := &ports.ChatEvent{
		Kind:           ports.ChatEventTurnCompleted,
		ProviderTurnID: t.current,
		NativeTurnID:   t.native,
		TurnState:      state,
		Err:            err,
	}
	t.closeLocked()
	return ev
}

// replyItem returns the item id assistant text is accumulated under this turn,
// adopting the provider's message id on first sight and otherwise a stable
// synthetic id so the controller folds every delta into one row.
func (t *turnTracker) replyItem(candidate, turn string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.replyItemID == "" {
		if candidate != "" {
			t.replyItemID = candidate
		} else {
			t.replyItemID = "zcode-reply:" + turn
		}
	}
	return t.replyItemID
}

// replyItemKnown reports whether any delta was streamed this turn.
func (t *turnTracker) replyItemKnown() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.replyItemID != ""
}

// progressDelta returns the part of a tool stream's new output tail that
// extends the previous one, and remembers the new tail. The key names the tool
// call and the stream (stdout and stderr are remembered separately). A tail
// that does not extend the last one yields "" (DESIGN 4.2: cumulative vs
// incremental is unknown).
func (t *turnTracker) progressDelta(key, tail string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	previous := t.toolTails[key]
	t.toolTails[key] = tail
	if !strings.HasPrefix(tail, previous) {
		return ""
	}
	return tail[len(previous):]
}

// active is the turn in flight as AO sees it: the open turn, or the prepared
// one whose session/send is written but whose prompt_started has not been
// pumped yet. An interrupt without a name targets it, and so does every
// notification or server request that arrives before the start signal is
// pumped (DESIGN 3.5: "the current pending turn").
func (t *turnTracker) active() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open {
		return t.current
	}
	return t.pending
}

// latchTextSource records which delta kind first carried text this turn and
// reports whether the given kind is the one to honour.
func (t *turnTracker) latchTextSource(kind string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.textSource == "" {
		t.textSource = kind
	}
	return t.textSource == kind
}

// nativeFor translates a minted id to the provider's turn id, or "".
func (t *turnTracker) nativeFor(minted string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nativeByMinted[minted]
}

// mintedFor translates a provider turn id to the AO id, or "".
func (t *turnTracker) mintedFor(native string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mintedByNative[native]
}

// currentTurn is the open turn's AO id, or "".
func (t *turnTracker) currentTurn() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open {
		return t.current
	}
	return ""
}
