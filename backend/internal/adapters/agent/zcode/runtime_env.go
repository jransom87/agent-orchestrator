package zcode

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.AgentRuntimeLaunchEnv = (*Plugin)(nil)

// AugmentRuntimeLaunchEnv pins the credential store the launched zcode process
// uses to the adapter-owned location under AO's data dir, so the auth probe
// (AuthStatus), the login plan, and every session launch resolve the same
// store. An explicitly pre-set ZCODE_DATA_BASE_DIR wins — that is the escape
// hatch for operator-managed stores, honored identically by the probe.
func (p *Plugin) AugmentRuntimeLaunchEnv(env map[string]string, dataDir string, _ domain.SessionID, _ string) {
	if env == nil {
		return
	}
	if _, ok := env[zcodeDataBaseDirEnv]; ok {
		return
	}
	if base := zcodeHarnessStoreBase(dataDir); base != "" {
		env[zcodeDataBaseDirEnv] = base
	}
}

// CredentialStoreBase returns the ZCODE_DATA_BASE_DIR value the adapter pins
// for a given AO data dir: <aoDataDir>/harnesses/zcode. Exported for the
// agentauth login plan, which must launch `zcode login zai` against the same
// store the probe reads and the sessions launch with.
func CredentialStoreBase(aoDataDir string) string {
	return zcodeHarnessStoreBase(aoDataDir)
}
