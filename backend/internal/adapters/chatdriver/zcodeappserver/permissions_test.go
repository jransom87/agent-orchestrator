package zcodeappserver

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/zcodeappserver/zcodeproto"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestPermissionModeMapping(t *testing.T) {
	cases := []struct {
		mode     ports.PermissionMode
		readOnly bool
		want     string
		wantErr  bool
	}{
		{ports.PermissionModeDefault, false, "edit", false},
		{"", false, "edit", false},
		{"unknown-mode", false, "edit", false},
		{ports.PermissionModeAcceptEdits, false, "edit", false},
		{ports.PermissionModeAuto, false, "build", false},
		{ports.PermissionModeBypassPermissions, false, "yolo", false},
		{ports.PermissionModeBypassPermissions, true, "", true},
		{ports.PermissionModeDefault, true, "", true},
	}
	for _, tc := range cases {
		got, err := sessionMode(tc.mode, tc.readOnly)
		if tc.wantErr {
			if !errors.Is(err, ports.ErrChatPermissionModeUnsupported) {
				t.Errorf("sessionMode(%q, readOnly=%v) err = %v, want ErrChatPermissionModeUnsupported", tc.mode, tc.readOnly, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("sessionMode(%q, %v) = %q, %v; want %q", tc.mode, tc.readOnly, got, err, tc.want)
		}
	}

	// The U5 guard: until interaction/requestPermission is proven, only
	// bypass-permissions starts, refused with the same sentinel kimiacp uses.
	workspace := t.TempDir()
	for _, mode := range []ports.PermissionMode{ports.PermissionModeDefault, ports.PermissionModeAcceptEdits, ports.PermissionModeAuto, ""} {
		if err := validateStart(workspace, mode, false); !errors.Is(err, ports.ErrChatPermissionModeUnsupported) {
			t.Errorf("validateStart(%q) = %v, want ErrChatPermissionModeUnsupported", mode, err)
		}
	}
	if err := validateStart(workspace, ports.PermissionModeBypassPermissions, false); err != nil {
		t.Errorf("validateStart(bypass) = %v", err)
	}
	if err := validateStart(workspace, ports.PermissionModeBypassPermissions, true); !errors.Is(err, ports.ErrChatPermissionModeUnsupported) {
		t.Errorf("validateStart(bypass, readOnly) = %v", err)
	}

	// The driver refuses before spawning anything.
	d, srv := newTestDriver(t)
	cfg := startConfig(t)
	cfg.Permissions = ports.PermissionModeAcceptEdits
	if _, err := d.Start(context.Background(), cfg); !errors.Is(err, ports.ErrChatPermissionModeUnsupported) {
		t.Fatalf("Start(accept-edits) = %v", err)
	}
	if len(srv.capturedEnv()) != 0 {
		t.Fatal("a refused mode spawned the provider")
	}
}

func TestPerTurnApprovalMustMatchLaunchMode(t *testing.T) {
	conv, _ := startConversation(t)
	if _, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{
		Text: "x", Settings: ports.ChatTurnSettings{Approval: ports.PermissionModeAcceptEdits},
	}); !errors.Is(err, ports.ErrChatPermissionModeUnsupported) {
		t.Fatalf("SendTurn with a different mode = %v", err)
	}
	if _, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{
		Text: "x", Settings: ports.ChatTurnSettings{Approval: ports.PermissionModeBypassPermissions},
	}); err != nil {
		t.Fatalf("SendTurn with the launch mode = %v", err)
	}
}

func TestDecisionKind(t *testing.T) {
	cases := []struct {
		resp zcodeproto.PermissionResponse
		want ports.ChatDecisionKind
	}{
		{zcodeproto.PermissionResponse{Decision: "allow"}, ports.ChatDecisionAllowOnce},
		{zcodeproto.PermissionResponse{Decision: "allow", PermissionUpdates: []byte(`[{"type":"addRules"}]`)}, ports.ChatDecisionAllowAlways},
		{zcodeproto.PermissionResponse{Decision: "allow", PermissionUpdates: []byte(`null`)}, ports.ChatDecisionAllowOnce},
		{zcodeproto.PermissionResponse{Decision: "deny"}, ports.ChatDecisionRejectOnce},
		{zcodeproto.PermissionResponse{Decision: "deny", PermissionUpdates: []byte(`[{}]`)}, ports.ChatDecisionRejectAlways},
		{zcodeproto.PermissionResponse{Decision: "modify"}, ports.ChatDecisionAllowOnce},
		{zcodeproto.PermissionResponse{Decision: "escalate"}, ports.ChatDecisionAllowOnce},
		{zcodeproto.PermissionResponse{Decision: "???"}, ""},
	}
	for _, tc := range cases {
		if got := decisionKind(tc.resp); got != tc.want {
			t.Errorf("decisionKind(%+v) = %q, want %q", tc.resp, got, tc.want)
		}
	}
}

func TestInputRequestFromQuestions(t *testing.T) {
	req := inputRequestFrom(zcodeproto.RequestUserInputParams{
		RequestID: "q1",
		Questions: []zcodeproto.Question{
			{Question: "Which?", Options: []zcodeproto.QuestionOption{{Value: "a"}, {Value: "b"}}},
			{Question: "Why?", Header: "why"},
		},
	})
	if req.Mode != ports.ChatInputModeForm || req.Message != "Which?" || req.ElicitationID != "q1" {
		t.Fatalf("request = %+v", req)
	}
	properties := req.Schema["properties"].(map[string]any)
	if _, ok := properties["Which?"]; !ok {
		t.Fatalf("question keyed by text missing: %+v", properties)
	}
	if _, ok := properties["why"]; !ok {
		t.Fatalf("question keyed by header missing: %+v", properties)
	}
	if enum := properties["Which?"].(map[string]any)["enum"].([]any); len(enum) != 2 {
		t.Fatalf("enum = %+v", enum)
	}
}
