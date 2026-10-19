package zcodeproto

import "encoding/json"

// Every params object the server accepts is zod `.strict()`: an unknown key
// rejects the frame. Optional fields therefore carry omitempty so an unset
// value is absent rather than null or "".

// Workspace identifies the session's working tree (`qXt.workspace`; PROVEN on
// session/create with workspacePath == workspaceKey == the --cwd).
type Workspace struct {
	WorkspacePath string `json:"workspacePath"`
	WorkspaceKey  string `json:"workspaceKey"`
}

// ModelSelection names a provider model (`mhr.ref`, `QXt.modelSelection`,
// `iQt.model`). Provider ids contain ':' (e.g. account:zai-individual-coding-plan);
// AO joins the two with '/' and splits on the last one. STATIC, U8.
type ModelSelection struct {
	ProviderID string          `json:"providerId"`
	ModelID    string          `json:"modelId"`
	Options    json.RawMessage `json:"options,omitempty"`
}

// MCPServerConfig is one stdio tool server on session/create (`the` union,
// stdio member). STATIC, U13.
type MCPServerConfig struct {
	Name      string     `json:"name"`
	Command   string     `json:"command"`
	Args      []string   `json:"args,omitempty"`
	Env       []EnvEntry `json:"env,omitempty"`
	Isolation string     `json:"isolation,omitempty"`
}

// EnvEntry is one {name, value} pair in MCPServerConfig.Env.
type EnvEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SessionCreateParams is session/create (`qXt`). PROVEN with only workspace
// set; mode, thoughtLevel, mcpServers, titleGenerationEnabled and persistence
// are STATIC. `model` is deliberately absent: sending one fails with
// "Provider Registry 中不存在 Model" (HANDOFF.md).
type SessionCreateParams struct {
	Workspace              Workspace         `json:"workspace"`
	Mode                   string            `json:"mode,omitempty"`
	ThoughtLevel           string            `json:"thoughtLevel,omitempty"`
	MCPServers             []MCPServerConfig `json:"mcpServers,omitempty"`
	TitleGenerationEnabled *bool             `json:"titleGenerationEnabled,omitempty"`
	Persistence            string            `json:"persistence,omitempty"`
}

// SessionSendParams is session/send (`QXt`). PROVEN with sessionId+content;
// inputId and modelSelection are STATIC (U3, U8). expectedRevision is never
// sent (DESIGN 3.6).
type SessionSendParams struct {
	SessionID      string          `json:"sessionId"`
	Content        string          `json:"content"`
	InputID        string          `json:"inputId,omitempty"`
	ModelSelection *ModelSelection `json:"modelSelection,omitempty"`
}

// SessionSendResult is the session/send acknowledgement (`Lhc`). PROVEN:
// {accepted:true, sessionId, stateRevision:1}.
type SessionSendResult struct {
	SessionID     string `json:"sessionId"`
	Accepted      bool   `json:"accepted"`
	StateRevision int64  `json:"stateRevision"`
}

// SessionStopParams is session/stop (`rQt`). Returns {} even when idle. STATIC, U4.
type SessionStopParams struct {
	SessionID string `json:"sessionId"`
}

// SessionSubscribeParams is session/subscribe (`$Xt`). STATIC, U1 (settled).
type SessionSubscribeParams struct {
	SessionID    string `json:"sessionId"`
	DeliveryKind string `json:"deliveryKind"`
}

// SessionSubscribeResult is the subscribe acknowledgement (`_ci`). STATIC.
type SessionSubscribeResult struct {
	SessionID string `json:"sessionId,omitempty"`
}

// SessionReadParams is session/read (`mci`). STATIC, U11.
type SessionReadParams struct {
	SessionID    string `json:"sessionId"`
	MessageLimit *int   `json:"messageLimit,omitempty"`
	DeliveryKind string `json:"deliveryKind,omitempty"`
}

// SessionResumeParams is session/resume (`WXt`). STATIC, U7.
type SessionResumeParams struct {
	SessionID string    `json:"sessionId"`
	Workspace Workspace `json:"workspace"`
}

// SessionSetModelParams is session/setModel (`iQt`). STATIC, U8.
type SessionSetModelParams struct {
	SessionID string         `json:"sessionId"`
	Model     ModelSelection `json:"model"`
}

// SessionSetModeParams is session/setMode. STATIC.
type SessionSetModeParams struct {
	SessionID string `json:"sessionId"`
	Mode      string `json:"mode"`
}

// SessionUsageParams is session/usage (`GYi`). STATIC, U9.
type SessionUsageParams struct {
	SessionID string `json:"sessionId"`
}

// SessionUsageResult is the session/usage result (`GYi`). STATIC, U9: the field
// names follow the AO mapping in DESIGN 4.2 and must be confirmed by P9.
type SessionUsageResult struct {
	InputTokens     int64 `json:"inputTokens"`
	OutputTokens    int64 `json:"outputTokens"`
	CacheReadTokens int64 `json:"cacheReadTokens"`
	TotalTokens     int64 `json:"totalTokens"`
}

// StateUpdatedParams is the state.updated notification (`Lhc`/`Xmc`). PROVEN
// as a progress signal; reason strings prompt_started / prompt_completed /
// prompt_failed are STATIC (verified in the bundle, never captured).
type StateUpdatedParams struct {
	Type      string          `json:"type,omitempty"`
	Scope     string          `json:"scope,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Revision  int64           `json:"revision,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Patch     json.RawMessage `json:"patch,omitempty"`
}

// OperationEventParams is computer-use/operation-event (`Oai`). STATIC. Sent
// before the deliveryKind gate, so it arrives even without a subscription.
type OperationEventParams struct {
	EventID        string `json:"eventId,omitempty"`
	SequenceNumber int64  `json:"sequenceNumber,omitempty"`
	SessionID      string `json:"sessionId,omitempty"`
	Timestamp      string `json:"timestamp,omitempty"`
	Kind           string `json:"kind"`
	TurnID         string `json:"turnId,omitempty"`
	ToolCallID     string `json:"toolCallId,omitempty"`
	ToolName       string `json:"toolName,omitempty"`
}

// SessionEventParams is session/event (`zXt`, `dhc`). STATIC. Only dispatched
// when the session record has a deliveryKind (U1, settled).
type SessionEventParams struct {
	EventID      string          `json:"eventId,omitempty"`
	SessionID    string          `json:"sessionId,omitempty"`
	TurnID       string          `json:"turnId,omitempty"`
	Seq          int64           `json:"seq,omitempty"`
	Timestamp    string          `json:"timestamp,omitempty"`
	DeliveryKind string          `json:"deliveryKind,omitempty"`
	Type         string          `json:"type"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

// session/event type strings (`zXt` discriminated union). STATIC.
const (
	EventTurnStarted         = "turn.started"
	EventTurnCompleted       = "turn.completed"
	EventTurnFailed          = "turn.failed"
	EventModelStreaming      = "model.streaming"
	EventPartDelta           = "part.delta"
	EventToolUpdated         = "tool.updated"
	EventPermissionRequested = "permission.requested"
	EventPermissionResolved  = "permission.resolved"
	EventUserInputResolved   = "userInput.resolved"
	EventSessionTitleUpdated = "session.titleUpdated"
	EventSessionClosed       = "session.closed"
)

// computer-use/operation-event kind strings (`Oai`). STATIC.
const (
	OperationTurnStarted   = "turn-started"
	OperationTurnCompleted = "turn-completed"
	OperationTurnFailed    = "turn-failed"
	OperationToolScheduled = "tool-scheduled"
	OperationToolStarted   = "tool-started"
	OperationSessionClosed = "session-closed"
)

// state.updated reason strings (`Lhc`, `Xmc`). STATIC.
const (
	ReasonPromptStarted   = "prompt_started"
	ReasonPromptCompleted = "prompt_completed"
	ReasonPromptFailed    = "prompt_failed"
)

// TurnStartedPayload is session/event turn.started (`Ehr`). STATIC, U3.
type TurnStartedPayload struct {
	InputID    string          `json:"inputId,omitempty"`
	TurnNumber int64           `json:"turnNumber,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
}

// TurnCompletedPayload is session/event turn.completed (`Dhr`). STATIC, U2/U4/U9.
type TurnCompletedPayload struct {
	Response   string          `json:"response,omitempty"`
	ResultType string          `json:"resultType,omitempty"`
	Usage      json.RawMessage `json:"usage,omitempty"`
	TokenCount int64           `json:"tokenCount,omitempty"`
	Duration   int64           `json:"duration,omitempty"`
}

// TurnUsage is the usage object AO reads off TurnCompletedPayload.Usage when it
// decodes. STATIC, U9.
type TurnUsage struct {
	InputTokens     int64 `json:"inputTokens"`
	OutputTokens    int64 `json:"outputTokens"`
	CacheReadTokens int64 `json:"cacheReadTokens"`
	TotalTokens     int64 `json:"totalTokens"`
}

// ProtocolErrorDetail is the error object on turn.failed (`UXt`, `D3`). STATIC.
type ProtocolErrorDetail struct {
	Type        string          `json:"type,omitempty"`
	Message     string          `json:"message,omitempty"`
	Detail      string          `json:"detail,omitempty"`
	Attribution json.RawMessage `json:"attribution,omitempty"`
}

// TurnFailedPayload is session/event turn.failed (`Ohr`). STATIC, U12.
type TurnFailedPayload struct {
	Error     ProtocolErrorDetail `json:"error"`
	TurnPhase string              `json:"turnPhase,omitempty"`
}

// ModelStreamingPayload is session/event model.streaming (`khr`). STATIC, U2.
type ModelStreamingPayload struct {
	Kind               string `json:"kind"`
	Delta              string `json:"delta,omitempty"`
	AssistantMessageID string `json:"assistantMessageId,omitempty"`
	PartID             string `json:"partId,omitempty"`
}

// model.streaming kind strings (`khr`). STATIC.
const (
	StreamingTextDelta      = "text_delta"
	StreamingReasoningStart = "reasoning_start"
	StreamingReasoningDelta = "reasoning_delta"
)

// PartDeltaPayload is session/event part.delta (`Lhr`). STATIC, U2.
type PartDeltaPayload struct {
	MessageID string `json:"messageId,omitempty"`
	PartID    string `json:"partId,omitempty"`
	Field     string `json:"field,omitempty"`
	Delta     string `json:"delta,omitempty"`
}

// ToolUpdatedPayload is session/event tool.updated (`jhr`), kind-discriminated
// with the optional fields flattened. STATIC.
type ToolUpdatedPayload struct {
	Kind          string          `json:"kind"`
	ToolCallID    string          `json:"toolCallId"`
	ToolName      string          `json:"toolName,omitempty"`
	Input         json.RawMessage `json:"input,omitempty"`
	OutputPreview string          `json:"outputPreview,omitempty"`
	StdoutTail    string          `json:"stdoutTail,omitempty"`
	StderrTail    string          `json:"stderrTail,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         json.RawMessage `json:"error,omitempty"`
	Duration      int64           `json:"duration,omitempty"`
}

// tool.updated kind strings (`jhr`). STATIC.
const (
	ToolScheduled = "scheduled"
	ToolStarted   = "started"
	ToolProgress  = "progress"
	ToolResult    = "result"
	ToolError     = "error"
)

// PermissionRequestedPayload is session/event permission.requested (`Bhr`).
// toolCallId is the required key. STATIC.
type PermissionRequestedPayload struct {
	RequestID  string `json:"requestId,omitempty"`
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName,omitempty"`
}

// PermissionResolvedPayload is session/event permission.resolved (`Uhr`:
// `requestId: yn.optional(), toolCallId: yn`). STATIC.
type PermissionResolvedPayload struct {
	RequestID  string `json:"requestId,omitempty"`
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName,omitempty"`
	Decision   string `json:"decision,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// UserInputResolvedPayload is session/event userInput.resolved (`$hr`). STATIC.
type UserInputResolvedPayload struct {
	RequestID string `json:"requestId,omitempty"`
	Action    string `json:"action,omitempty"`
}

// TitleUpdatedPayload is session/event session.titleUpdated (`Chr`). STATIC.
type TitleUpdatedPayload struct {
	Title string `json:"title"`
}

// RequestRuntimePreferencesParams is the server request raised inside
// session/create (`KYi`). PROVEN: params.sessionId is the session id source.
type RequestRuntimePreferencesParams struct {
	SessionID string `json:"sessionId"`
}

// RequestRuntimePreferencesResult is the reply (`XXt`, strict). Both keys are
// always emitted: nativeSearchEnhancementsEnabled is PROVEN; the second is
// STATIC and must be false so the server does not self-answer AskUserQuestion
// (U6).
type RequestRuntimePreferencesResult struct {
	NativeSearchEnhancementsEnabled      bool `json:"nativeSearchEnhancementsEnabled"`
	AskUserQuestionAutoResolutionEnabled bool `json:"askUserQuestionAutoResolutionEnabled"`
}

// PermissionOption is one entry of RequestPermissionParams.Options (`fXi`).
// Response is echoed verbatim as the reply result. STATIC, U5.
type PermissionOption struct {
	OptionID    string          `json:"optionId"`
	Kind        string          `json:"kind,omitempty"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Response    json.RawMessage `json:"response"`
}

// RequestPermissionParams is interaction/requestPermission (`fXi`). STATIC, U5.
type RequestPermissionParams struct {
	RequestID  string             `json:"requestId"`
	SessionID  string             `json:"sessionId,omitempty"`
	TurnID     string             `json:"turnId,omitempty"`
	ToolCallID string             `json:"toolCallId"`
	ToolName   string             `json:"toolName,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	RiskLevel  string             `json:"riskLevel,omitempty"`
	Input      json.RawMessage    `json:"input,omitempty"`
	Options    []PermissionOption `json:"options"`
}

// PermissionResponse is the shape of PermissionOption.Response (`_F`, strict).
// AO reads it to classify the decision; it never rebuilds it. STATIC, U5.
type PermissionResponse struct {
	Decision          string          `json:"decision"`
	Reason            string          `json:"reason,omitempty"`
	ModifiedInput     json.RawMessage `json:"modifiedInput,omitempty"`
	PermissionUpdates json.RawMessage `json:"permissionUpdates,omitempty"`
}

// QuestionOption is one choice of a Question (`mXi`). STATIC, U6.
type QuestionOption struct {
	Value       string `json:"value"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// Question is one entry of RequestUserInputParams.Questions (`mXi`). STATIC, U6.
type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	Options     []QuestionOption `json:"options,omitempty"`
	MultiSelect bool             `json:"multiSelect,omitempty"`
}

// RequestUserInputParams is interaction/requestUserInput (`mXi`). STATIC, U6.
type RequestUserInputParams struct {
	RequestID string          `json:"requestId"`
	SessionID string          `json:"sessionId,omitempty"`
	TurnID    string          `json:"turnId,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	Prompt    string          `json:"prompt,omitempty"`
	Questions []Question      `json:"questions,omitempty"`
	Schema    json.RawMessage `json:"schema,omitempty"`
}

// UserInputResponse is the reply to interaction/requestUserInput (`oDe`,
// strict). Content is only sent on accept. STATIC, U6.
type UserInputResponse struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitempty"`
	Reason  string         `json:"reason,omitempty"`
}

// RequestProviderRuntimeHeadersParams is interaction/requestProviderRuntimeHeaders
// (`hXi`). STATIC, U0: whether it is sent at all decides DESIGN 7.1. The
// payload is kept raw beyond the two keys the probe is told to record.
type RequestProviderRuntimeHeadersParams struct {
	SessionID     string          `json:"sessionId,omitempty"`
	ProviderID    string          `json:"providerId,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	AccountAccess json.RawMessage `json:"accountAccess,omitempty"`
}

// RequestProviderRuntimeHeadersResult is the reply shape read off `Coi`. The
// required-field set is unknown (U0); a resolver fills what it knows.
type RequestProviderRuntimeHeadersResult struct {
	HeadersApplied bool            `json:"headersApplied"`
	RequestAuth    json.RawMessage `json:"requestAuth,omitempty"`
}

// RequestOfficialMcpAuthHeadersParams is interaction/requestOfficialMcpAuthHeaders
// (`_Xi`). AO declines it; the params are kept for logging only. STATIC.
type RequestOfficialMcpAuthHeadersParams struct {
	SessionID string `json:"sessionId,omitempty"`
	ServerID  string `json:"serverId,omitempty"`
}

// SubagentRuntimeConfigResult answers subagents/readRuntimeConfig (`Net`), which
// is never sent unless the client enabled it. STATIC.
type SubagentRuntimeConfigResult struct {
	Kind string `json:"kind"`
}

// AvailableModel is one entry of Snapshot.Settings.Model.Available (`mhr`).
// STATIC, U8.
type AvailableModel struct {
	Ref           ModelSelection  `json:"ref"`
	Label         string          `json:"label,omitempty"`
	ContextWindow int64           `json:"contextWindow,omitempty"`
	Reasoning     *ModelReasoning `json:"reasoning,omitempty"`
}

// ModelReasoning is the reasoning block of an AvailableModel (`mhr`). STATIC.
type ModelReasoning struct {
	Levels []string `json:"levels,omitempty"`
}

// ModelSettings is Snapshot.Settings.Model (`mhr`). STATIC, U8.
type ModelSettings struct {
	Current   *ModelSelection  `json:"current,omitempty"`
	Available []AvailableModel `json:"available,omitempty"`
	LastUsed  *ModelSelection  `json:"lastUsed,omitempty"`
}

// SnapshotSession is Snapshot.Session (`JQe`/`rDe`). The key is sessionId; there
// is no `id` (the probe's result.session.id fallback never ran). STATIC.
type SnapshotSession struct {
	SessionID string          `json:"sessionId"`
	Mode      string          `json:"mode,omitempty"`
	Status    string          `json:"status,omitempty"`
	Model     *ModelSelection `json:"model,omitempty"`
}

// SnapshotSettings is Snapshot.Settings. STATIC.
type SnapshotSettings struct {
	Model        ModelSettings   `json:"model"`
	ThoughtLevel json.RawMessage `json:"thoughtLevel,omitempty"`
	Mode         json.RawMessage `json:"mode,omitempty"`
}

// SnapshotProjection is Snapshot.Projection (`Shr`). STATIC.
type SnapshotProjection struct {
	Status             string          `json:"status,omitempty"`
	ContextUsed        int64           `json:"contextUsed,omitempty"`
	ContextWindow      int64           `json:"contextWindow,omitempty"`
	CurrentTurnID      string          `json:"currentTurnId,omitempty"`
	PendingPermissions json.RawMessage `json:"pendingPermissions,omitempty"`
	ActiveToolCalls    json.RawMessage `json:"activeToolCalls,omitempty"`
	BackgroundJobs     json.RawMessage `json:"backgroundJobs,omitempty"`
}

// SnapshotRuntime is Snapshot.Runtime (`xZt`). pendingRequestIds is what a live
// reconnect reconciles host-replayed server requests against. STATIC.
type SnapshotRuntime struct {
	EventSeq          int64    `json:"eventSeq,omitempty"`
	StateRevision     int64    `json:"stateRevision,omitempty"`
	DeliveryKind      string   `json:"deliveryKind,omitempty"`
	ActiveTurnID      string   `json:"activeTurnId,omitempty"`
	PendingRequestIDs []string `json:"pendingRequestIds,omitempty"`
}

// MessageInfo is Message.Info (`ZQe`). STATIC, U11.
type MessageInfo struct {
	Role      string `json:"role,omitempty"`
	MessageID string `json:"messageId,omitempty"`
}

// Part is one entry of Message.Parts (`KQe`). STATIC, U11.
type Part struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// Message is one entry of Snapshot.Messages (`NLn`). STATIC, U11.
type Message struct {
	Info  MessageInfo `json:"info"`
	Parts []Part      `json:"parts,omitempty"`
}

// Snapshot is the full session snapshot returned by session/create,
// session/resume, session/read and session/setModel (`JQe`). The create
// result's key set (messages, projection, protocol, runtime, session, settings)
// is PROVEN; the nested shapes are STATIC.
type Snapshot struct {
	Protocol   json.RawMessage    `json:"protocol,omitempty"`
	Session    SnapshotSession    `json:"session"`
	Settings   SnapshotSettings   `json:"settings"`
	Projection SnapshotProjection `json:"projection"`
	Runtime    SnapshotRuntime    `json:"runtime"`
	Messages   []Message          `json:"messages,omitempty"`
}
