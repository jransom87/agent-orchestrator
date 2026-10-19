package zcodeappserver

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/zcodeappserver/zcodeproto"
)

// Does the driver still agree with the catalog about the protocol?
//
// zcode ships no schema generator, so the catalog is hand-maintained and this
// table is the cross-check: every method the driver sends or reads is named
// here, and every one must be declared with a provenance. Adding a method to
// the driver without adding it here is the mistake this table makes visible.
var driverMethods = []string{
	// Outbound.
	zcodeproto.MethodSessionCreate,
	zcodeproto.MethodSessionSend,
	zcodeproto.MethodSessionSubscribe,
	zcodeproto.MethodSessionStop,
	zcodeproto.MethodSessionResume,
	zcodeproto.MethodSessionRead,
	zcodeproto.MethodSessionSetModel,
	zcodeproto.MethodSessionUsage,

	// Server -> client requests the router answers.
	zcodeproto.MethodSessionRequestRuntimePreferences,
	zcodeproto.MethodInteractionRequestPermission,
	zcodeproto.MethodInteractionRequestUserInput,
	zcodeproto.MethodInteractionRequestProviderRuntimeHeaders,
	zcodeproto.MethodInteractionRequestOfficialMcpAuthHeaders,
	zcodeproto.MethodSubagentsReadRuntimeConfig,

	// Notifications the normalizer reads.
	zcodeproto.MethodStateUpdated,
	zcodeproto.MethodComputerUseOperationEvent,
	zcodeproto.MethodSessionEvent,
	zcodeproto.MethodInteractionProviderRuntimeHeadersCancelled,

	// Notifications the normalizer drops by name.
	zcodeproto.MethodStartupStorageState,
	zcodeproto.MethodProcessResourceSample,
	zcodeproto.MethodProcessMcpTelemetry,
	zcodeproto.MethodProcessMcpResourceSamples,
	zcodeproto.MethodProcessToolExecResource,
	zcodeproto.MethodProcessNetworkRequests,
	zcodeproto.MethodPluginsOperationProgress,
}

func TestEveryMethodTheDriverUsesIsDeclared(t *testing.T) {
	for _, method := range driverMethods {
		info, ok := zcodeproto.Info(method)
		if !ok || !zcodeproto.Declares(method) {
			t.Errorf("%s is used by the driver but not declared in zcodeproto.Methods", method)
			continue
		}
		if info.Provenance != zcodeproto.ProvenanceProven && info.Provenance != zcodeproto.ProvenanceStatic {
			t.Errorf("%s has provenance %q, want PROVEN or STATIC", method, info.Provenance)
		}
		if info.Symbol == "" {
			t.Errorf("%s names no bundle symbol", method)
		}
	}
	for _, info := range zcodeproto.Methods {
		found := false
		for _, method := range driverMethods {
			if method == info.Name {
				found = true
				break
			}
		}
		if !found && info.Name != zcodeproto.MethodSessionSetMode {
			t.Errorf("%s is declared but the driver table does not claim it", info.Name)
		}
	}
}

// TestProvenMethodsHaveReplayFixtures demands a capture for every method the
// catalog calls PROVEN, so a row cannot be promoted without the evidence
// landing in testdata.
func TestProvenMethodsHaveReplayFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ndjson") {
			continue
		}
		for _, line := range loadReplay(t, entry.Name()) {
			if line.Frame.Method != "" {
				seen[line.Frame.Method] = true
			}
		}
	}
	for _, info := range zcodeproto.Methods {
		if info.Provenance != zcodeproto.ProvenanceProven {
			continue
		}
		if !seen[info.Name] {
			t.Errorf("%s is PROVEN but appears in no testdata/*.ndjson capture", info.Name)
		}
	}
}

// TestBundleDigestMatchesInstalledProvider is the analog of codex's generated
// schema check. zcode cannot emit a schema, so a digest mismatch only warns
// (the cue to re-run the Phase 1 captures); a method name missing from the
// bundle is a failure, because the driver would then be speaking to nothing.
func TestBundleDigestMatchesInstalledProvider(t *testing.T) {
	bundle, err := os.ReadFile(filepath.Clean(zcodeproto.BundlePath))
	if err != nil {
		t.Skipf("zcode bundle not installed at %s: %v", zcodeproto.BundlePath, err)
	}
	sum := sha256.Sum256(bundle)
	if digest := hex.EncodeToString(sum[:]); digest != zcodeproto.BundleSHA256 {
		t.Logf("installed zcode bundle digest %s differs from the catalog's %s (%s): re-run the Phase 1 captures before trusting STATIC rows",
			digest, zcodeproto.BundleSHA256, zcodeproto.ProviderVersion)
	}
	for _, info := range zcodeproto.Methods {
		if !strings.Contains(string(bundle), `"`+info.Name+`"`) {
			t.Errorf("method %s is not present in the installed bundle", info.Name)
		}
	}
}
