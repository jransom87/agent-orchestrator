package zcodeappserver_test

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Phase 0 keeps the driver out of registry.Build: a registered driver whose
// Probe succeeds changes spawn behaviour (the manager only falls back to TUI
// on a typed Probe error), and the capability floor is unproven until the
// Phase 1 captures land. Phase 2 replaces this test with the shipped-list
// entry, so the flip is a deliberate test change.
func TestZcodeIsNotShippedYet(t *testing.T) {
	r := registry.Build(nil, nil)
	if r.SupportsChat(domain.HarnessZcode) {
		t.Fatal("zcode has a shipped chat driver before Phase 2")
	}
	if _, err := r.Driver(domain.HarnessZcode); !errors.Is(err, ports.ErrChatUnsupported) {
		t.Fatalf("Driver(zcode) = %v, want ErrChatUnsupported", err)
	}
}
