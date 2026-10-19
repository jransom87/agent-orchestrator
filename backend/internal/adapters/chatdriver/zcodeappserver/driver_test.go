package zcodeappserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/zcode"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/zcodeappserver/zcodeproto"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// fakePlugin is kimiacp's fake extended with a no-op AugmentRuntimeLaunchEnv
// that only records its arguments. It never writes ZCODE_DATA_BASE_DIR, so the
// one test that asserts on that key uses the real plugin.
type fakePlugin struct {
	binary  string
	binErr  error
	status  ports.AgentAuthStatus
	authErr error

	mu       sync.Mutex
	augments []augmentCall
}

type augmentCall struct {
	dataDir   string
	sessionID domain.SessionID
}

func (p *fakePlugin) ResolveBinary(context.Context) (string, error) { return p.binary, p.binErr }
func (p *fakePlugin) AuthStatus(context.Context) (ports.AgentAuthStatus, error) {
	return p.status, p.authErr
}
func (p *fakePlugin) AugmentRuntimeLaunchEnv(_ map[string]string, dataDir string, sessionID domain.SessionID, _ string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.augments = append(p.augments, augmentCall{dataDir: dataDir, sessionID: sessionID})
}

// testSnapshot is a STATIC-shaped session snapshot used as the canned
// session/create, session/resume and session/read result.
const testSnapshot = `{"protocol":{},"session":{"sessionId":"sess-1","mode":"yolo"},` +
	`"settings":{"model":{"current":{"providerId":"account:zai-individual-coding-plan","modelId":"GLM-5.3"},` +
	`"available":[{"ref":{"providerId":"account:zai-individual-coding-plan","modelId":"GLM-5.3"},"label":"GLM 5.3","contextWindow":200000,"reasoning":{"levels":["low","high"]}},` +
	`{"ref":{"providerId":"account:zai-individual-coding-plan","modelId":"GLM-5.3-Flash"},"label":"GLM 5.3 Flash"}]}},` +
	`"projection":{"contextUsed":1200,"contextWindow":200000},"runtime":{"pendingRequestIds":[]},"messages":[]}`

// scriptedServer stands in for `zcode app-server` over in-memory pipes. It
// answers client requests from a canned table, lets a test push notifications
// and server->client requests, and — unlike codex's fixture — can hold a
// request's reply until the driver has answered a server request the handler
// raised in the meantime (onRequest + awaitReply), which is the proven create
// exchange.
type scriptedServer struct {
	t        *testing.T
	toClient io.WriteCloser

	mu        sync.Mutex
	responses map[string]string
	failures  map[string]string
	handlers  map[string]func(id, params json.RawMessage)
	seen      []frame
	changed   chan struct{}

	envMu sync.Mutex
	env   []string
}

func (s *scriptedServer) respondTo(method, resultJSON string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[method] = resultJSON
	delete(s.failures, method)
	delete(s.handlers, method)
}

func (s *scriptedServer) replyError(method string, code int, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[method] = `{"code":` + strconv.Itoa(code) + `,"message":` + strconv.Quote(message) + `}`
	delete(s.handlers, method)
}

// onRequest installs a handler that runs on its own goroutine when the client
// sends the method. The handler owns the reply (see respond).
func (s *scriptedServer) onRequest(method string, fn func(id, params json.RawMessage)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = fn
}

func (s *scriptedServer) push(raw string) {
	s.t.Helper()
	if _, err := io.WriteString(s.toClient, raw+"\n"); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		// A closed pipe is teardown racing a scripted handler's trailing
		// frames (noise after a create result, say), not a failed expectation.
		s.t.Errorf("push: %v", err)
	}
}

func (s *scriptedServer) respond(id json.RawMessage, resultJSON string) {
	s.push(`{"id":` + string(id) + `,"result":` + resultJSON + `}`)
}

func (s *scriptedServer) record(f frame) {
	s.mu.Lock()
	s.seen = append(s.seen, f)
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func (s *scriptedServer) frames() []frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]frame(nil), s.seen...)
}

func (s *scriptedServer) sentMethod(method string) bool {
	for _, f := range s.frames() {
		if f.Method == method {
			return true
		}
	}
	return false
}

func (s *scriptedServer) countFrames(pred func(frame) bool) int {
	n := 0
	for _, f := range s.frames() {
		if pred(f) {
			n++
		}
	}
	return n
}

// awaitFrame waits for a frame matching pred among everything the client sent.
func (s *scriptedServer) awaitFrame(pred func(frame) bool) frame {
	s.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		for _, f := range s.seen {
			if pred(f) {
				s.mu.Unlock()
				return f
			}
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			s.t.Fatal("timed out waiting for an expected client frame")
			return frame{}
		}
	}
}

// awaitReply waits for the driver's reply to a server->client request id.
func (s *scriptedServer) awaitReply(id string) frame {
	s.t.Helper()
	return s.awaitFrame(func(f frame) bool {
		return f.Method == "" && f.ID != nil && rawID(*f.ID) == id
	})
}

func (s *scriptedServer) awaitRequest(method string) frame {
	s.t.Helper()
	return s.awaitFrame(func(f frame) bool { return f.Method == method && f.ID != nil })
}

func (s *scriptedServer) serve(serverReads io.Reader) {
	br := bufio.NewReader(serverReads)
	for {
		line, err := readFrame(br)
		if err != nil {
			return
		}
		if len(line) == 0 {
			continue
		}
		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			continue
		}
		s.record(f)
		if f.ID == nil || f.Method == "" {
			continue
		}
		s.mu.Lock()
		handler := s.handlers[f.Method]
		reply, known := s.responses[f.Method]
		failure, refused := s.failures[f.Method]
		s.mu.Unlock()
		id := *f.ID
		switch {
		case handler != nil:
			go handler(id, f.Params)
		case refused:
			s.push(`{"id":` + string(id) + `,"error":` + failure + `}`)
		case known:
			s.respond(id, reply)
		}
	}
}

func (s *scriptedServer) capturedEnv() []string {
	s.envMu.Lock()
	defer s.envMu.Unlock()
	return append([]string(nil), s.env...)
}

// newScriptedServer wires the pipes and the default canned table. The returned
// process is what the injected spawn hands the driver.
func newScriptedServer(t *testing.T) (*scriptedServer, *process) {
	t.Helper()
	clientReads, serverWrites := io.Pipe()
	serverReads, clientWrites := io.Pipe()
	srv := &scriptedServer{
		t:        t,
		toClient: serverWrites,
		responses: map[string]string{
			zcodeproto.MethodSessionSubscribe: `{"sessionId":"sess-1"}`,
			zcodeproto.MethodSessionSend:      `{"accepted":true,"sessionId":"sess-1","stateRevision":1}`,
			zcodeproto.MethodSessionStop:      `{}`,
			zcodeproto.MethodSessionResume:    testSnapshot,
			zcodeproto.MethodSessionRead:      testSnapshot,
			zcodeproto.MethodSessionSetModel:  testSnapshot,
		},
		failures: map[string]string{},
		handlers: map[string]func(id, params json.RawMessage){},
		changed:  make(chan struct{}),
	}
	// The default create exchange: raise the runtime-preferences request, hold
	// the create result until it is answered, then write the snapshot.
	srv.onRequest(zcodeproto.MethodSessionCreate, func(id, _ json.RawMessage) {
		srv.push(`{"id":"server-1","method":"session/requestRuntimePreferences","params":{"sessionId":"sess-1"}}`)
		srv.awaitReply("server-1")
		srv.respond(id, testSnapshot)
	})
	go srv.serve(serverReads)
	proc := &process{
		stdin:  clientWrites,
		stdout: clientReads,
		stop:   func() error { return serverWrites.Close() },
	}
	t.Cleanup(func() { _ = serverWrites.Close() })
	return srv, proc
}

// newTestDriver wires a Driver to a scripted server over in-memory pipes, so no
// process is ever spawned. The env the driver computed is captured.
func newTestDriver(t *testing.T) (*Driver, *scriptedServer) {
	t.Helper()
	return newTestDriverWithPlugin(t, &fakePlugin{binary: "zcode", status: ports.AgentAuthStatusAuthorized})
}

func newTestDriverWithPlugin(t *testing.T, plugin zcodePlugin) (*Driver, *scriptedServer) {
	t.Helper()
	srv, proc := newScriptedServer(t)
	d := &Driver{
		plugin: plugin,
		log:    slog.New(slog.DiscardHandler),
		spawn: func(_ context.Context, _, _ string, env []string) (*process, error) {
			srv.envMu.Lock()
			srv.env = append([]string(nil), env...)
			srv.envMu.Unlock()
			return proc, nil
		},
		runtimeHeaders: declineResolver{},
	}
	return d, srv
}

// newHostTestDriver wires the persistent-host path with an injected connectHost
// so the live-reconnect branch runs over the same pipes.
func newHostTestDriver(t *testing.T, reconnected bool, nextRequestID int64) (*Driver, *scriptedServer, *[]persistenthost.Config) {
	t.Helper()
	srv, proc := newScriptedServer(t)
	var configs []persistenthost.Config
	d := &Driver{
		plugin:     &fakePlugin{binary: "zcode", status: ports.AgentAuthStatusAuthorized},
		log:        slog.New(slog.DiscardHandler),
		persistent: true,
		connectHost: func(_ context.Context, cfg persistenthost.Config) (*persistenthost.Transport, error) {
			configs = append(configs, cfg)
			return &persistenthost.Transport{
				Stdin: proc.stdin, Stdout: proc.stdout, Reconnected: reconnected, NextRequestID: nextRequestID,
			}, nil
		},
		runtimeHeaders: declineResolver{},
	}
	return d, srv, &configs
}

func startConfig(t *testing.T) ports.ChatStartConfig {
	t.Helper()
	return ports.ChatStartConfig{
		SessionID:     "ao-1",
		DataDir:       t.TempDir(),
		WorkspacePath: t.TempDir(),
		Permissions:   ports.PermissionModeBypassPermissions,
	}
}

// startConversation opens a conversation on the default scripted exchange.
func startConversation(t *testing.T) (*conversation, *scriptedServer) {
	t.Helper()
	d, srv := newTestDriver(t)
	conv, err := d.Start(context.Background(), startConfig(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = conv.Close() })
	return conv.(*conversation), srv
}

// sendTurn prepares and starts one turn, returning the minted id.
func sendTurn(t *testing.T, conv *conversation, text string) string {
	t.Helper()
	ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: text})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if err := conv.StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("StartDeferredTurn: %v", err)
	}
	return ref.ProviderTurnID
}

// nextEvent returns the next event of the wanted kind, skipping others.
func nextEvent(t *testing.T, events <-chan ports.ChatEvent, want ports.ChatEventKind) ports.ChatEvent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("event stream closed while waiting for %q", want)
			}
			if ev.Kind == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %q", want)
			return ports.ChatEvent{}
		}
	}
}

// drainEvents collects whatever arrives within the window.
func drainEvents(events <-chan ports.ChatEvent, window time.Duration) []ports.ChatEvent {
	var out []ports.ChatEvent
	deadline := time.After(window)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
}

func countKind(events []ports.ChatEvent, kind ports.ChatEventKind) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func paramsMap(t *testing.T, f frame) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(f.Params, &m); err != nil {
		t.Fatalf("params of %s not an object: %v", f.Method, err)
	}
	return m
}

// --- replay loader -----------------------------------------------------------

// replayLine is one direction-tagged line of a testdata/*.ndjson capture:
// "out" is what the client wrote, "in" what the server wrote.
type replayLine struct {
	Dir   string `json:"dir"`
	Frame frame  `json:"frame"`
	Note  string `json:"note"`
}

func loadReplay(t *testing.T, name string) []replayLine {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var lines []replayLine
	for i, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var line replayLine
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("%s line %d: %v", name, i+1, err)
		}
		if line.Dir != "in" && line.Dir != "out" {
			t.Fatalf("%s line %d: dir %q", name, i+1, line.Dir)
		}
		lines = append(lines, line)
	}
	return lines
}

func isRequest(f frame) bool { return f.ID != nil && f.Method != "" }

// replay drives the onRequest/awaitReply hooks from a capture: every "out"
// request becomes a handler that writes the "in" lines that followed it, holds
// the response until each "out" reply to a server request has arrived, and
// checks that reply byte-for-byte against the capture.
func (s *scriptedServer) replay(lines []replayLine) {
	s.t.Helper()
	for i := 0; i < len(lines); i++ {
		if lines[i].Dir != "out" || !isRequest(lines[i].Frame) {
			continue
		}
		start := i + 1
		end := start
		for end < len(lines) && !(lines[end].Dir == "out" && isRequest(lines[end].Frame)) {
			end++
		}
		group := lines[start:end]
		s.onRequest(lines[i].Frame.Method, func(id, _ json.RawMessage) {
			for _, line := range group {
				switch {
				case line.Dir == "in" && isRequest(line.Frame):
					s.push(marshalFrame(s.t, line.Frame))
				case line.Dir == "in" && line.Frame.ID != nil:
					// The capture's numeric id is replaced with the driver's own.
					f := line.Frame
					f.ID = &id
					s.push(marshalFrame(s.t, f))
				case line.Dir == "in":
					s.push(marshalFrame(s.t, line.Frame))
				case line.Dir == "out" && line.Frame.ID != nil && line.Frame.Method == "":
					got := s.awaitReply(rawID(*line.Frame.ID))
					if !jsonEqual(got.Result, line.Frame.Result) {
						s.t.Errorf("reply to %s = %s, capture has %s", rawID(*line.Frame.ID), got.Result, line.Frame.Result)
					}
				}
			}
		})
		i = end - 1
	}
}

func marshalFrame(t *testing.T, f frame) string {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	return string(b)
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// --- driver / conversation tests ---------------------------------------------

func TestStartCreatesSessionAnswersRuntimePreferencesAndSubscribes(t *testing.T) {
	d, srv := newTestDriver(t)
	srv.replay(loadReplay(t, "proven-create-send.ndjson"))

	conv, err := d.Start(context.Background(), startConfig(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = conv.Close() }()

	create := srv.awaitRequest(zcodeproto.MethodSessionCreate)
	params := paramsMap(t, create)
	if _, ok := params["workspace"]; !ok {
		t.Fatalf("session/create params lack workspace: %s", create.Params)
	}
	if _, ok := params["model"]; ok {
		t.Fatalf("session/create must omit model (HANDOFF.md:57-60): %s", create.Params)
	}
	reply := srv.awaitReply("server-1")
	if want := `{"nativeSearchEnhancementsEnabled":true,"askUserQuestionAutoResolutionEnabled":false}`; string(reply.Result) != want {
		t.Fatalf("runtime preferences reply = %s, want %s", reply.Result, want)
	}
	// The reply must precede the create result; the replay holds the result
	// until it sees the reply, so Start returning proves the order. Pin it
	// on the frame log too.
	frames := srv.frames()
	replyIndex, createIndex := -1, -1
	for i, f := range frames {
		if f.Method == "" && f.ID != nil && rawID(*f.ID) == "server-1" {
			replyIndex = i
		}
		if f.Method == zcodeproto.MethodSessionCreate {
			createIndex = i
		}
	}
	if replyIndex < 0 || createIndex < 0 || replyIndex < createIndex {
		t.Fatalf("expected create request before preferences reply, got frames %+v", frames)
	}
	if got := conv.ProviderConversationID(); got != "sess-proven-1" {
		t.Fatalf("ProviderConversationID = %q, want the params.sessionId of the preferences request", got)
	}
	subscribe := srv.awaitRequest(zcodeproto.MethodSessionSubscribe)
	if got := paramsMap(t, subscribe)["deliveryKind"]; got != deliveryKind {
		t.Fatalf("subscribe deliveryKind = %v, want %q", got, deliveryKind)
	}
	if srv.sentMethod(zcodeproto.MethodSessionResume) {
		t.Fatal("Start must not send session/resume")
	}

	// The proven send: prompt_started precedes the ack and maps to the minted id.
	c := conv.(*conversation)
	id := sendTurn(t, c, "Reply with exactly the word OK and nothing else.")
	send := srv.awaitRequest(zcodeproto.MethodSessionSend)
	if got := paramsMap(t, send)["content"]; got != "Reply with exactly the word OK and nothing else." {
		t.Fatalf("send content = %v", got)
	}
	started := nextEvent(t, conv.Events(), ports.ChatEventTurnStarted)
	if started.ProviderTurnID != id {
		t.Fatalf("turn.started id = %q, want minted %q", started.ProviderTurnID, id)
	}
}

// storePinningPlugin is the real zcode plugin for AugmentRuntimeLaunchEnv with
// the binary and auth answers fixed, so the test neither execs nor reads a
// credential store.
type storePinningPlugin struct{ *zcode.Plugin }

func (storePinningPlugin) ResolveBinary(context.Context) (string, error) { return "zcode", nil }
func (storePinningPlugin) AuthStatus(context.Context) (ports.AgentAuthStatus, error) {
	return ports.AgentAuthStatusAuthorized, nil
}

func TestStartInjectsCredentialStoreEnv(t *testing.T) {
	t.Setenv("ZCODE_DATA_BASE_DIR", "")

	t.Run("pins the AO-owned store", func(t *testing.T) {
		d, srv := newTestDriverWithPlugin(t, storePinningPlugin{zcode.New()})
		cfg := startConfig(t)
		conv, err := d.Start(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = conv.Close() }()
		want := "ZCODE_DATA_BASE_DIR=" + zcode.CredentialStoreBase(cfg.DataDir)
		if !containsEnv(srv.capturedEnv(), want) {
			t.Fatalf("spawn env lacks %q", want)
		}
	})

	t.Run("overlay value wins", func(t *testing.T) {
		d, srv := newTestDriverWithPlugin(t, storePinningPlugin{zcode.New()})
		cfg := startConfig(t)
		override := filepath.Join(t.TempDir(), "override-store")
		cfg.Env = map[string]string{"ZCODE_DATA_BASE_DIR": override}
		conv, err := d.Start(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = conv.Close() }()
		if !containsEnv(srv.capturedEnv(), "ZCODE_DATA_BASE_DIR="+override) {
			t.Fatalf("spawn env lacks the overlay value")
		}
		if containsEnv(srv.capturedEnv(), "ZCODE_DATA_BASE_DIR="+zcode.CredentialStoreBase(cfg.DataDir)) {
			t.Fatalf("spawn env carries the default store despite an overlay")
		}
	})
}

func containsEnv(env []string, entry string) bool {
	for _, e := range env {
		if e == entry {
			return true
		}
	}
	return false
}

func TestStartPassesDataDirAndSessionToThePlugin(t *testing.T) {
	plugin := &fakePlugin{binary: "zcode", status: ports.AgentAuthStatusAuthorized}
	d, _ := newTestDriverWithPlugin(t, plugin)
	cfg := startConfig(t)
	conv, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = conv.Close() }()
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if len(plugin.augments) != 1 || plugin.augments[0].dataDir != cfg.DataDir || plugin.augments[0].sessionID != cfg.SessionID {
		t.Fatalf("AugmentRuntimeLaunchEnv calls = %+v", plugin.augments)
	}
}

func TestStartRejectsRelativeWorkspacePath(t *testing.T) {
	d, srv := newTestDriver(t)
	cfg := startConfig(t)
	cfg.WorkspacePath = "relative/ws"
	if _, err := d.Start(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Start err = %v, want an absolute-path refusal", err)
	}
	if len(srv.capturedEnv()) != 0 {
		t.Fatal("a rejected start must not spawn")
	}
}

func TestProbeReportsAuthRequired(t *testing.T) {
	d, _ := newTestDriverWithPlugin(t, &fakePlugin{binary: "zcode", status: ports.AgentAuthStatusUnauthorized})
	if _, err := d.Probe(context.Background()); !errors.Is(err, ports.ErrChatAuthRequired) {
		t.Fatalf("Probe err = %v, want ErrChatAuthRequired", err)
	}
	// Layer 2: Start re-checks because a cached Probe outlives the token.
	if _, err := d.Start(context.Background(), startConfig(t)); !errors.Is(err, ports.ErrChatAuthRequired) {
		t.Fatalf("Start err = %v, want ErrChatAuthRequired", err)
	}
}

func TestProbeReportsBinaryMissing(t *testing.T) {
	d, _ := newTestDriverWithPlugin(t, &fakePlugin{binErr: errors.New("no zcode wrapper"), status: ports.AgentAuthStatusAuthorized})
	if _, err := d.Probe(context.Background()); !errors.Is(err, ports.ErrChatDriverUnavailable) {
		t.Fatalf("Probe err = %v, want ErrChatDriverUnavailable", err)
	}
}

func TestProbeReportsCapabilitiesAndHarness(t *testing.T) {
	d, _ := newTestDriver(t)
	if d.Harness() != domain.HarnessZcode {
		t.Fatalf("Harness = %q", d.Harness())
	}
	caps, err := d.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !caps.Has(ports.ChatCapabilityInterrupt) || !caps.Has(ports.ChatCapabilityResume) {
		t.Fatalf("capabilities = %v", caps)
	}
}

func TestSendTurnIsDeferredAndCarriesInputID(t *testing.T) {
	conv, srv := startConversation(t)
	ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "hello"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if ref.ProviderTurnID == "" {
		t.Fatal("SendTurn minted no id")
	}
	time.Sleep(50 * time.Millisecond)
	if srv.sentMethod(zcodeproto.MethodSessionSend) {
		t.Fatal("SendTurn wrote session/send before StartDeferredTurn")
	}
	if err := conv.StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("StartDeferredTurn: %v", err)
	}
	send := srv.awaitRequest(zcodeproto.MethodSessionSend)
	params := paramsMap(t, send)
	if params["inputId"] != ref.ProviderTurnID || params["content"] != "hello" || params["sessionId"] != "sess-1" {
		t.Fatalf("session/send params = %s", send.Params)
	}
	if _, ok := params["expectedRevision"]; ok {
		t.Fatal("expectedRevision must be omitted (DESIGN 3.6)")
	}
}

func TestSendTurnRejectsEmptyTextAndBusyConversation(t *testing.T) {
	conv, _ := startConversation(t)
	if _, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "  "}); err == nil {
		t.Fatal("empty text accepted")
	}
	if _, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "a"}); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if _, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "b"}); err == nil {
		t.Fatal("second SendTurn accepted while one is prepared")
	}
}

func TestPromptConflictFailsTheDeferredStart(t *testing.T) {
	conv, srv := startConversation(t)
	srv.replyError(zcodeproto.MethodSessionSend, zcodeproto.CodePromptConflict, "A prompt is already running for this session")

	ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "hello"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	err = conv.StartDeferredTurn(ref.ProviderTurnID)
	if err == nil || !strings.Contains(err.Error(), "A prompt is already running") {
		t.Fatalf("StartDeferredTurn err = %v, want the provider's message verbatim", err)
	}
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || rpcErr.Code != zcodeproto.CodePromptConflict {
		t.Fatalf("error does not carry the -32010 code: %v", err)
	}
	if errors.Is(err, ports.ErrChatAuthRequired) {
		t.Fatal("a prompt conflict must never read as an auth failure")
	}
	if got := drainEvents(conv.Events(), 100*time.Millisecond); countKind(got, ports.ChatEventTurnStarted) != 0 {
		t.Fatalf("turn.started emitted for a rejected send: %+v", got)
	}

	// The tracker was disarmed: a second turn is accepted and maps correctly.
	srv.respondTo(zcodeproto.MethodSessionSend, `{"accepted":true,"sessionId":"sess-1","stateRevision":2}`)
	second := sendTurn(t, conv, "again")
	srv.push(`{"method":"state.updated","params":{"reason":"prompt_started","sessionId":"sess-1"}}`)
	started := nextEvent(t, conv.Events(), ports.ChatEventTurnStarted)
	if started.ProviderTurnID != second {
		t.Fatalf("turn.started id = %q, want %q", started.ProviderTurnID, second)
	}
}

func TestPromptStartedBeforeAckStillMapsToMintedID(t *testing.T) {
	conv, srv := startConversation(t)
	srv.onRequest(zcodeproto.MethodSessionSend, func(id, _ json.RawMessage) {
		// The real wire order (DESIGN 3.6): the notification precedes the ack.
		srv.push(`{"method":"state.updated","params":{"type":"state.updated","scope":"session","sessionId":"sess-1","revision":2,"reason":"prompt_started","patch":{"status":"running"}}}`)
		srv.respond(id, `{"accepted":true,"sessionId":"sess-1","stateRevision":2}`)
	})
	id := sendTurn(t, conv, "hello")
	got := drainEvents(conv.Events(), 150*time.Millisecond)
	if n := countKind(got, ports.ChatEventTurnStarted); n != 1 {
		t.Fatalf("turn.started count = %d, events %+v", n, got)
	}
	for _, ev := range got {
		if ev.Kind == ports.ChatEventTurnStarted && ev.ProviderTurnID != id {
			t.Fatalf("turn.started id = %q, want minted %q", ev.ProviderTurnID, id)
		}
		if strings.HasPrefix(ev.ProviderTurnID, providerTurnPrefix) {
			t.Fatalf("provider-initiated id minted for AO's own turn: %+v", ev)
		}
	}
}

func TestDiscardDeferredTurnDisarmsTracker(t *testing.T) {
	conv, srv := startConversation(t)
	ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "first"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	conv.DiscardDeferredTurn(ref.ProviderTurnID)
	if err := conv.StartDeferredTurn(ref.ProviderTurnID); err == nil {
		t.Fatal("a discarded turn started")
	}
	second := sendTurn(t, conv, "second")
	srv.push(`{"method":"state.updated","params":{"reason":"prompt_started"}}`)
	started := nextEvent(t, conv.Events(), ports.ChatEventTurnStarted)
	if started.ProviderTurnID != second {
		t.Fatalf("turn.started id = %q, want %q (not the discarded %q)", started.ProviderTurnID, second, ref.ProviderTurnID)
	}
}

func completeTurn(t *testing.T, conv *conversation, srv *scriptedServer) {
	t.Helper()
	srv.push(`{"method":"state.updated","params":{"reason":"prompt_started"}}`)
	nextEvent(t, conv.Events(), ports.ChatEventTurnStarted)
	srv.push(`{"method":"state.updated","params":{"reason":"prompt_completed"}}`)
	nextEvent(t, conv.Events(), ports.ChatEventTurnCompleted)
}

func TestStandingInstructionsLeadTheFirstTurnOnly(t *testing.T) {
	d, srv := newTestDriver(t)
	cfg := startConfig(t)
	cfg.SystemPrompt = "You are the worker."
	started, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = started.Close() }()
	conv := started.(*conversation)

	sendTurn(t, conv, "task one")
	first := srv.awaitRequest(zcodeproto.MethodSessionSend)
	if got := paramsMap(t, first)["content"]; got != "You are the worker.\n\ntask one" {
		t.Fatalf("first content = %q", got)
	}
	completeTurn(t, conv, srv)

	sendTurn(t, conv, "task two")
	srv.awaitFrame(func(f frame) bool {
		return f.Method == zcodeproto.MethodSessionSend && strings.Contains(string(f.Params), "task two")
	})
	if n := srv.countFrames(func(f frame) bool {
		return f.Method == zcodeproto.MethodSessionSend && strings.Contains(string(f.Params), "You are the worker.")
	}); n != 1 {
		t.Fatalf("preamble sent %d times, want once", n)
	}

	// A resume with prior turns never re-sends the role.
	d2, srv2 := newTestDriver(t)
	resumed, err := d2.Resume(context.Background(), ports.ChatResumeConfig{
		SessionID: "ao-1", ProviderConversationID: "sess-1", DataDir: t.TempDir(), WorkspacePath: t.TempDir(),
		Permissions: ports.PermissionModeBypassPermissions, SystemPrompt: "You are the worker.", HasPriorTurns: true,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer func() { _ = resumed.Close() }()
	sendTurn(t, resumed.(*conversation), "task three")
	send := srv2.awaitRequest(zcodeproto.MethodSessionSend)
	if got := paramsMap(t, send)["content"]; got != "task three" {
		t.Fatalf("resumed content = %q, want bare", got)
	}
}

func TestTurnLifecycleFromStateUpdatedOnly(t *testing.T) {
	conv, srv := startConversation(t)
	id := sendTurn(t, conv, "hello")
	srv.push(`{"method":"state.updated","params":{"reason":"prompt_started","patch":{"status":"running"}}}`)
	srv.push(`{"method":"state.updated","params":{"reason":"prompt_completed","patch":{}}}`)
	got := drainEvents(conv.Events(), 150*time.Millisecond)
	if countKind(got, ports.ChatEventTurnStarted) != 1 || countKind(got, ports.ChatEventTurnCompleted) != 1 {
		t.Fatalf("events = %+v", got)
	}
	if countKind(got, ports.ChatEventMessageCompleted) != 0 {
		t.Fatal("message.completed emitted with no subscription text (the no-subscribe floor has none)")
	}
	for _, ev := range got {
		if ev.Kind == ports.ChatEventTurnCompleted && (ev.ProviderTurnID != id || ev.TurnState != domain.TurnStateCompleted) {
			t.Fatalf("turn.completed = %+v", ev)
		}
	}
}

func TestReplyTextFromTurnCompletedResponse(t *testing.T) {
	conv, srv := startConversation(t)
	id := sendTurn(t, conv, "hello")
	srv.push(`{"method":"state.updated","params":{"reason":"prompt_started"}}`)
	nextEvent(t, conv.Events(), ports.ChatEventTurnStarted)
	srv.push(`{"method":"session/event","params":{"type":"turn.completed","turnId":"t-native","sessionId":"sess-1","payload":{"response":"OK","resultType":"success"}}}`)
	message := nextEvent(t, conv.Events(), ports.ChatEventMessageCompleted)
	if message.Text != "OK" || message.ProviderTurnID != id {
		t.Fatalf("message.completed = %+v", message)
	}
	completed := nextEvent(t, conv.Events(), ports.ChatEventTurnCompleted)
	if completed.TurnState != domain.TurnStateCompleted || completed.ProviderTurnID != id {
		t.Fatalf("turn.completed = %+v", completed)
	}
	// The prompt_* fallback must not settle the turn a second time.
	srv.push(`{"method":"state.updated","params":{"reason":"prompt_completed"}}`)
	if got := drainEvents(conv.Events(), 100*time.Millisecond); countKind(got, ports.ChatEventTurnCompleted) != 0 {
		t.Fatalf("turn settled twice: %+v", got)
	}
}

func TestInterruptSendsSessionStop(t *testing.T) {
	conv, srv := startConversation(t)
	id := sendTurn(t, conv, "hello")
	srv.push(`{"method":"state.updated","params":{"reason":"prompt_started"}}`)
	nextEvent(t, conv.Events(), ports.ChatEventTurnStarted)
	if err := conv.Interrupt(context.Background(), id); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	stop := srv.awaitRequest(zcodeproto.MethodSessionStop)
	if got := paramsMap(t, stop)["sessionId"]; got != "sess-1" {
		t.Fatalf("session/stop params = %s", stop.Params)
	}
}

func TestInterruptWithoutActiveTurnReturnsNoActiveTurn(t *testing.T) {
	conv, srv := startConversation(t)
	if err := conv.Interrupt(context.Background(), ""); !errors.Is(err, ports.ErrChatNoActiveTurn) {
		t.Fatalf("Interrupt err = %v, want ErrChatNoActiveTurn", err)
	}
	if srv.sentMethod(zcodeproto.MethodSessionStop) {
		t.Fatal("session/stop written with no active turn")
	}
}

func TestInterruptedTurnSettlesInterrupted(t *testing.T) {
	conv, srv := startConversation(t)
	id := sendTurn(t, conv, "count slowly")
	srv.push(`{"method":"state.updated","params":{"reason":"prompt_started"}}`)
	nextEvent(t, conv.Events(), ports.ChatEventTurnStarted)
	if err := conv.Interrupt(context.Background(), ""); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	srv.push(`{"method":"state.updated","params":{"reason":"prompt_failed"}}`)
	completed := nextEvent(t, conv.Events(), ports.ChatEventTurnCompleted)
	if completed.TurnState != domain.TurnStateInterrupted || completed.ProviderTurnID != id {
		t.Fatalf("turn.completed = %+v, want interrupted", completed)
	}
}

func TestResumeSendsSessionResumeAndNeverCreates(t *testing.T) {
	d, srv := newTestDriver(t)
	conv, err := d.Resume(context.Background(), ports.ChatResumeConfig{
		SessionID: "ao-1", ProviderConversationID: "sess-1", DataDir: t.TempDir(), WorkspacePath: t.TempDir(),
		Permissions: ports.PermissionModeBypassPermissions,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer func() { _ = conv.Close() }()
	resume := srv.awaitRequest(zcodeproto.MethodSessionResume)
	params := paramsMap(t, resume)
	if params["sessionId"] != "sess-1" {
		t.Fatalf("session/resume params = %s", resume.Params)
	}
	if _, ok := params["workspace"]; !ok {
		t.Fatalf("session/resume lacks workspace: %s", resume.Params)
	}
	srv.awaitRequest(zcodeproto.MethodSessionSubscribe)
	if srv.sentMethod(zcodeproto.MethodSessionCreate) {
		t.Fatal("Resume sent session/create")
	}
	if conv.ProviderConversationID() != "sess-1" {
		t.Fatalf("ProviderConversationID = %q", conv.ProviderConversationID())
	}
	if conv.(*conversation).ReconnectedLive() {
		t.Fatal("a replacement-process resume reported as live reconnect")
	}
}

func TestResumeFailureDoesNotFallBackToCreate(t *testing.T) {
	d, srv := newTestDriver(t)
	srv.replyError(zcodeproto.MethodSessionResume, zcodeproto.CodeSessionUnavailable, "Session not found: sess-gone")
	_, err := d.Resume(context.Background(), ports.ChatResumeConfig{
		SessionID: "ao-1", ProviderConversationID: "sess-gone", DataDir: t.TempDir(), WorkspacePath: t.TempDir(),
		Permissions: ports.PermissionModeBypassPermissions,
	})
	if !errors.Is(err, ports.ErrChatResumeFailed) {
		t.Fatalf("Resume err = %v, want ErrChatResumeFailed", err)
	}
	if srv.sentMethod(zcodeproto.MethodSessionCreate) {
		t.Fatal("Resume fell back to session/create")
	}
}

func TestResumeFreshIfMissingCreatesOnlyOnSessionNotFound(t *testing.T) {
	t.Run("not found creates", func(t *testing.T) {
		d, srv := newTestDriver(t)
		srv.replyError(zcodeproto.MethodSessionResume, zcodeproto.CodeSessionUnavailable, "Session not found: sess-gone")
		conv, err := d.Resume(context.Background(), ports.ChatResumeConfig{
			SessionID: "ao-1", ProviderConversationID: "sess-gone", FreshIfMissing: true,
			DataDir: t.TempDir(), WorkspacePath: t.TempDir(), Permissions: ports.PermissionModeBypassPermissions,
		})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		defer func() { _ = conv.Close() }()
		srv.awaitRequest(zcodeproto.MethodSessionCreate)
		if conv.ProviderConversationID() != "sess-1" {
			t.Fatalf("fresh session id = %q", conv.ProviderConversationID())
		}
	})
	t.Run("not active is not missing", func(t *testing.T) {
		d, srv := newTestDriver(t)
		srv.replyError(zcodeproto.MethodSessionResume, zcodeproto.CodeSessionUnavailable, "Session is not active: sess-1")
		_, err := d.Resume(context.Background(), ports.ChatResumeConfig{
			SessionID: "ao-1", ProviderConversationID: "sess-1", FreshIfMissing: true,
			DataDir: t.TempDir(), WorkspacePath: t.TempDir(), Permissions: ports.PermissionModeBypassPermissions,
		})
		if !errors.Is(err, ports.ErrChatResumeFailed) {
			t.Fatalf("Resume err = %v, want ErrChatResumeFailed", err)
		}
		if srv.sentMethod(zcodeproto.MethodSessionCreate) {
			t.Fatal("eviction treated as a missing session")
		}
	})
}

func TestReconnectSkipsCreateResumeAndSubscribe(t *testing.T) {
	d, srv, configs := newHostTestDriver(t, true, 41)
	conv, err := d.Reconnect(context.Background(), ports.ChatResumeConfig{
		SessionID: "ao-1", ProviderConversationID: "sess-1", DataDir: t.TempDir(), WorkspacePath: t.TempDir(),
		Permissions: ports.PermissionModeBypassPermissions,
	})
	if err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	defer func() { _ = conv.Close() }()
	if len(*configs) != 1 || !(*configs)[0].ReconnectOnly || (*configs)[0].Protocol != persistenthost.ProtocolRaw {
		t.Fatalf("host config = %+v", *configs)
	}
	if !conv.(*conversation).ReconnectedLive() || !conv.(*conversation).PreservesProviderOnClose() {
		t.Fatal("live reconnect not reported")
	}
	read := srv.awaitRequest(zcodeproto.MethodSessionRead)
	if rawID(*read.ID) != "42" {
		t.Fatalf("first request id after reconnect = %s, want 42 (NextRequestID+1)", *read.ID)
	}
	for _, method := range []string{zcodeproto.MethodSessionCreate, zcodeproto.MethodSessionResume, zcodeproto.MethodSessionSubscribe} {
		if srv.sentMethod(method) {
			t.Fatalf("live reconnect sent %s", method)
		}
	}
	sendTurn(t, conv.(*conversation), "hello")
	send := srv.awaitRequest(zcodeproto.MethodSessionSend)
	if rawID(*send.ID) != "43" {
		t.Fatalf("send id = %s, want 43", *send.ID)
	}
}

func TestPersistentStartUsesTheHostWithProvenArgv(t *testing.T) {
	d, _, configs := newHostTestDriver(t, false, 0)
	cfg := startConfig(t)
	conv, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = conv.Close() }()
	got := (*configs)[0]
	want := []string{"zcode", "app-server", "--stdio", "--cwd", cfg.WorkspacePath}
	if !reflect.DeepEqual(got.Argv, want) {
		t.Fatalf("argv = %v, want %v", got.Argv, want)
	}
	if got.Protocol != persistenthost.ProtocolRaw || got.SessionID != "ao-1" || got.Workdir != cfg.WorkspacePath {
		t.Fatalf("host config = %+v", got)
	}
}

func TestControllerStopIsAnnounced(t *testing.T) {
	conv, srv := startConversation(t)
	srv.push(`{"id":"server-9","method":"interaction/requestPermission","params":{"requestId":"r1","toolCallId":"tc1","toolName":"Bash","options":[{"optionId":"allow","name":"Allow","response":{"decision":"allow"}}]}}`)
	nextEvent(t, conv.Events(), ports.ChatEventApprovalRequested)

	_ = srv.toClient.Close()

	stopped := nextEvent(t, conv.Events(), ports.ChatEventControllerState)
	for stopped.ControllerState != ports.ChatControllerStopped {
		stopped = nextEvent(t, conv.Events(), ports.ChatEventControllerState)
	}
	if _, ok := <-conv.Events(); ok {
		t.Fatal("Events() still open after stop")
	}
	if err := conv.ResolveRequest(context.Background(), "r1", ports.ChatDecision{ID: "allow"}); !errors.Is(err, errConversationClosed) {
		t.Fatalf("ResolveRequest after stop = %v, want closed", err)
	}
}

func TestCloseClosesEventsExactlyOnce(t *testing.T) {
	conv, _ := startConversation(t)
	if err := conv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := conv.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-conv.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("Events() did not close after Close")
		}
	}
}
