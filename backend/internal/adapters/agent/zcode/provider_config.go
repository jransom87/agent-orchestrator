package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/authprobe"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/hookutil"
)

const (
	// zcodePersonalProviderConfigEnv points the CLI at a personal provider
	// config file instead of ~/.zcode/v2/provider_config.json. AO sets it to
	// the adapter-owned file below so the coding-plan API key is used without
	// an OAuth login.
	zcodePersonalProviderConfigEnv = "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE"
	// personalProviderConfigFileName is the AO-owned provider config file
	// name under the harness store base.
	personalProviderConfigFileName = "provider_config.json"

	// Provider identity written into the AO-owned config. The shape is the
	// one proven live (HANDOFF.md, Phase 1 run 3): a personal `zai-api`
	// template provider needs personalModelIds or it lists no models.
	providerConfigSchemaVersion = 1
	providerConfigProviderID    = "zai-api-ao"
	providerConfigTemplateID    = "zai-api"
	providerConfigProviderName  = "Z.ai Coding Plan (AO)"
	providerConfigGroup         = "standard-personal"
	providerConfigAccessType    = "zhipu-coding-plan-api-key"
	providerConfigDefaultModel  = "GLM-5.3"
)

// providerConfigModelIDs lists the coding-plan models the AO provider exposes.
var providerConfigModelIDs = []string{"GLM-5.3", "GLM-5.3-Flash"}

// PersonalProviderConfigPath returns the AO-owned personal provider config
// file for a given AO data dir: <aoDataDir>/harnesses/zcode/provider_config.json,
// beside the credential store CredentialStoreBase names. Exported for the
// agentauth login plan, which passes it to the hidden `ao zcode-login`
// command so the key lands where the probe and the launches look. Empty when
// the data dir is unknown.
func PersonalProviderConfigPath(aoDataDir string) string {
	base := zcodeHarnessStoreBase(aoDataDir)
	if base == "" {
		return ""
	}
	return filepath.Join(base, personalProviderConfigFileName)
}

// personalProviderConfig mirrors the subset of the CLI's provider config
// document AO writes. Field order matches the proven file.
type personalProviderConfig struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Config        personalProviderRules `json:"config"`
}

type personalProviderRules struct {
	ProviderConfigRules   providerConfigRules   `json:"providerConfigRules"`
	ModelConfigRules      modelConfigRules      `json:"modelConfigRules"`
	DefaultModelSelection defaultModelSelection `json:"defaultModelSelection"`
}

type providerConfigRules struct {
	ProviderRules []providerRule `json:"providerRules"`
}

type providerRule struct {
	ProviderID   string             `json:"providerId"`
	TemplateID   string             `json:"templateId"`
	ProviderName string             `json:"providerName"`
	Config       providerRuleConfig `json:"config"`
}

type providerRuleConfig struct {
	Group            string         `json:"group"`
	Access           providerAccess `json:"access"`
	PersonalModelIDs []string       `json:"personalModelIds"`
}

type providerAccess struct {
	Type   string `json:"type"`
	APIKey string `json:"apiKey"`
}

type modelConfigRules struct {
	ProviderModelRules       []any `json:"providerModelRules"`
	ManualProviderModelRules []any `json:"manualProviderModelRules"`
}

type defaultModelSelection struct {
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`
}

// WritePersonalProviderConfig writes the AO-owned personal provider config
// carrying apiKey to path: the directory is created 0700 and the file is
// written atomically with mode 0600. The key is never logged or returned;
// the caller owns and zeroes apiKey. An empty key is refused.
func WritePersonalProviderConfig(ctx context.Context, path string, apiKey []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("zcode: provider config path is required")
	}
	if len(bytes.TrimSpace(apiKey)) == 0 {
		return fmt.Errorf("zcode: API key is required")
	}
	doc := personalProviderConfig{
		SchemaVersion: providerConfigSchemaVersion,
		Config: personalProviderRules{
			ProviderConfigRules: providerConfigRules{ProviderRules: []providerRule{{
				ProviderID:   providerConfigProviderID,
				TemplateID:   providerConfigTemplateID,
				ProviderName: providerConfigProviderName,
				Config: providerRuleConfig{
					Group:            providerConfigGroup,
					Access:           providerAccess{Type: providerConfigAccessType, APIKey: string(bytes.TrimSpace(apiKey))},
					PersonalModelIDs: append([]string(nil), providerConfigModelIDs...),
				},
			}}},
			ModelConfigRules:      modelConfigRules{ProviderModelRules: []any{}, ManualProviderModelRules: []any{}},
			DefaultModelSelection: defaultModelSelection{ProviderID: providerConfigProviderID, ModelID: providerConfigDefaultModel},
		},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("zcode: encode provider config: %w", err)
	}
	data := buf.Bytes()
	defer func() {
		for i := range data {
			data[i] = 0
		}
	}()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("zcode: create provider config dir: %w", err)
	}
	if err := hookutil.AtomicWriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("zcode: write provider config: %w", err)
	}
	return nil
}

// providerConfigHasKey reports whether the AO-owned provider config at path
// holds a provider rule with a non-empty API key. A missing file is (false,
// nil); unreadable or invalid JSON records a credential-store diagnostic and
// returns the error. The key's value is never retained or recorded.
func providerConfigHasKey(ctx context.Context, path string) (bool, error) {
	data, err := authprobe.ReadCredentialFile(ctx, path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return false, nil
	}
	var doc personalProviderConfig
	if err := json.Unmarshal(data, &doc); err != nil {
		authprobe.RecordFileDiagnostic(ctx, path, "credential_file_invalid")
		return false, fmt.Errorf("zcode: parse provider config: %w", err)
	}
	for _, rule := range doc.Config.ProviderConfigRules.ProviderRules {
		if strings.TrimSpace(rule.Config.Access.APIKey) != "" {
			return true, nil
		}
	}
	return false, nil
}

// personalProviderConfigExists reports whether the AO-owned provider config
// is a regular file the launched CLI could read.
func personalProviderConfigExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
