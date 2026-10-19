package zcodeappserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/zcodeappserver/zcodeproto"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestListModelsReadsSnapshotSettings(t *testing.T) {
	conv, _ := startConversation(t)
	models, err := conv.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	if models[0].ID != "account:zai-individual-coding-plan/GLM-5.3" || !models[0].Default || models[0].DisplayName != "GLM 5.3" {
		t.Fatalf("models[0] = %+v", models[0])
	}
	if models[0].Efforts != nil {
		t.Fatalf("efforts = %v advertised, but zcode has no per-turn thought level (U8)", models[0].Efforts)
	}
	if models[1].ID != "account:zai-individual-coding-plan/GLM-5.3-Flash" || models[1].Default {
		t.Fatalf("models[1] = %+v", models[1])
	}
}

func TestPerTurnEffortIsRefused(t *testing.T) {
	d, srv := newTestDriver(t)
	cfg := startConfig(t)
	cfg.Effort = "low"
	started, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = started.Close() }()
	if got := paramsMap(t, srv.awaitRequest(zcodeproto.MethodSessionCreate))["thoughtLevel"]; got != "low" {
		t.Fatalf("session/create thoughtLevel = %v, want the launch effort", got)
	}
	conv := started.(*conversation)
	if _, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{
		Text: "x", Settings: ports.ChatTurnSettings{Effort: "high"},
	}); !errors.Is(err, ports.ErrChatCapabilityUnavailable) {
		t.Fatalf("SendTurn with a per-turn effort = %v, want ErrChatCapabilityUnavailable", err)
	}
	// The launch level, which is what every turn runs under anyway, is accepted.
	if _, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{
		Text: "x", Settings: ports.ChatTurnSettings{Effort: "low"},
	}); err != nil {
		t.Fatalf("SendTurn with the launch effort = %v", err)
	}
}

func TestListModelsWithoutSnapshotIsEmpty(t *testing.T) {
	conv := &conversation{}
	models, err := conv.ListModels(context.Background())
	if err != nil || len(models) != 0 {
		t.Fatalf("ListModels = %+v, %v", models, err)
	}
}

func TestParseModelSelectionSplitsOnLastSlash(t *testing.T) {
	cases := []struct {
		id       string
		provider string
		model    string
		ok       bool
	}{
		{"account:zai-individual-coding-plan/GLM-5.3", "account:zai-individual-coding-plan", "GLM-5.3", true},
		{"vendor/sub/model-x", "vendor/sub", "model-x", true},
		{"no-slash", "", "", false},
		{"/leading", "", "", false},
		{"trailing/", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		got, ok := parseModelSelection(tc.id)
		if ok != tc.ok || got.ProviderID != tc.provider || got.ModelID != tc.model {
			t.Errorf("parseModelSelection(%q) = %+v, %v; want %q/%q, %v", tc.id, got, ok, tc.provider, tc.model, tc.ok)
		}
	}
	if modelID(zcodeproto.ModelSelection{ProviderID: "a:b", ModelID: "c"}) != "a:b/c" {
		t.Fatal("modelID join")
	}
}

func sendWithModel(model string) ports.ChatUserMessage {
	return ports.ChatUserMessage{Text: "which model?", Settings: ports.ChatTurnSettings{Model: model}}
}

func TestSendTurnCarriesModelSelection(t *testing.T) {
	conv, srv := startConversation(t)
	ref, err := conv.SendTurn(context.Background(), sendWithModel("account:zai-individual-coding-plan/GLM-5.3-Flash"))
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if err := conv.StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("StartDeferredTurn: %v", err)
	}
	send := srv.awaitRequest(zcodeproto.MethodSessionSend)
	var params zcodeproto.SessionSendParams
	if err := json.Unmarshal(send.Params, &params); err != nil {
		t.Fatalf("params: %v", err)
	}
	if params.ModelSelection == nil || params.ModelSelection.ProviderID != "account:zai-individual-coding-plan" || params.ModelSelection.ModelID != "GLM-5.3-Flash" {
		t.Fatalf("modelSelection = %+v", params.ModelSelection)
	}
}

func TestSetModelSendsSessionSetModel(t *testing.T) {
	conv, srv := startConversation(t)
	if err := conv.SetModel(context.Background(), "account:zai-individual-coding-plan/GLM-5.3-Flash"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	set := srv.awaitRequest(zcodeproto.MethodSessionSetModel)
	var params zcodeproto.SessionSetModelParams
	if err := json.Unmarshal(set.Params, &params); err != nil {
		t.Fatalf("params: %v", err)
	}
	if params.SessionID != "sess-1" || params.Model.ModelID != "GLM-5.3-Flash" {
		t.Fatalf("params = %+v", params)
	}
	if err := conv.SetModel(context.Background(), "bogus"); err == nil {
		t.Fatal("bogus id accepted")
	}
}
