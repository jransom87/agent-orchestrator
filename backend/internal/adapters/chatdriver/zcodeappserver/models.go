package zcodeappserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/zcodeappserver/zcodeproto"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// modelIDSeparator joins providerId and modelId into AO's single model id.
// Provider ids contain ':' (account:zai-individual-coding-plan), so the join
// uses '/' and the split takes the LAST one.
const modelIDSeparator = "/"

// modelID is the AO id for a provider model selection.
func modelID(selection zcodeproto.ModelSelection) string {
	return selection.ProviderID + modelIDSeparator + selection.ModelID
}

// parseModelSelection splits an AO model id back into the provider pair.
func parseModelSelection(id string) (zcodeproto.ModelSelection, bool) {
	cut := strings.LastIndex(id, modelIDSeparator)
	if cut <= 0 || cut == len(id)-1 {
		return zcodeproto.ModelSelection{}, false
	}
	return zcodeproto.ModelSelection{ProviderID: id[:cut], ModelID: id[cut+1:]}, true
}

// ListModels reads the catalog off the latest session snapshot
// (settings.model.available, STATIC U8). The snapshot is the only honest
// source: zcode has no model/list endpoint, and the create/resume result is
// where the account's entitlement shows up.
func (c *conversation) ListModels(context.Context) ([]ports.ChatModel, error) {
	c.mu.Lock()
	settings := c.snapshot.Settings.Model
	c.mu.Unlock()

	current := ""
	if settings.Current != nil {
		current = modelID(*settings.Current)
	}
	models := make([]ports.ChatModel, 0, len(settings.Available))
	for _, entry := range settings.Available {
		if entry.Ref.ProviderID == "" || entry.Ref.ModelID == "" {
			continue
		}
		id := modelID(entry.Ref)
		display := entry.Label
		if display == "" {
			display = entry.Ref.ModelID
		}
		var efforts []string
		if entry.Reasoning != nil {
			efforts = append(efforts, entry.Reasoning.Levels...)
		}
		models = append(models, ports.ChatModel{
			ID: id, DisplayName: display, Default: id == current, Efforts: efforts,
		})
	}
	return models, nil
}

// SetModel changes the session's durable model through session/setModel
// (STATIC, U8). The returned snapshot replaces the cached one so ListModels
// reflects the change. Not wired to a port interface until Phase 1 proves it.
func (c *conversation) SetModel(ctx context.Context, id string) error {
	selection, ok := parseModelSelection(id)
	if !ok {
		return fmt.Errorf("%w: model id %q is not providerId/modelId", ports.ErrChatConfigOptionInvalid, id)
	}
	var snapshot zcodeproto.Snapshot
	if err := c.conn.request(ctx, zcodeproto.MethodSessionSetModel, zcodeproto.SessionSetModelParams{
		SessionID: c.sessionID, Model: selection,
	}, &snapshot); err != nil {
		return fmt.Errorf("%s: %w", zcodeproto.MethodSessionSetModel, err)
	}
	c.mu.Lock()
	c.snapshot = snapshot
	c.mu.Unlock()
	return nil
}
