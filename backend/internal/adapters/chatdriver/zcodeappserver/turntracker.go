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

// cancel disarms a prepared id after the session/send request failed. It
// reports whether turn.started had already been emitted for that id, in which
// case the caller must emit a failed turn.completed so the controller cannot
// stay Busy.
func (t *turnTracker) cancel(id string) (started bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == id {
		t.pending = ""
	}
	if t.open && t.current == id {
		t.open = false
		t.current = ""
		t.native = ""
		t.textSource = ""
		t.replyItemID = ""
		return true
	}
	return false
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
	t.open = true
	t.current = id
	t.native = native
	t.textSource = ""
	t.replyItemID = ""
	t.toolTails = make(map[string]string)
	if native != "" {
		t.nativeByMinted[id] = native
		t.mintedByNative[native] = id
	}
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
	t.open = false
	t.current = ""
	t.native = ""
	t.textSource = ""
	t.replyItemID = ""
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

// progressDelta returns the part of a tool's new output tail that extends the
// previous one, and remembers the new tail. A tail that does not extend the
// last one yields "" (DESIGN 4.2: cumulative vs incremental is unknown).
func (t *turnTracker) progressDelta(toolCallID, tail string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	previous := t.toolTails[toolCallID]
	t.toolTails[toolCallID] = tail
	if !strings.HasPrefix(tail, previous) {
		return ""
	}
	return tail[len(previous):]
}

// active is the turn an interrupt without a name targets: the open turn, or
// the prepared one whose session/send was acknowledged but whose
// prompt_started has not been pumped yet.
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
