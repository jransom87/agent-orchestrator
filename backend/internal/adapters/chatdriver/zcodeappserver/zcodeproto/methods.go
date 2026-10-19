package zcodeproto

// Method names the driver sends or reads, in both directions. Each has a
// provenance tag in Methods; see doc.go for what PROVEN and STATIC mean.
const (
	// Client -> server requests.
	MethodSessionCreate    = "session/create"
	MethodSessionSend      = "session/send"
	MethodSessionSubscribe = "session/subscribe"
	MethodSessionStop      = "session/stop"
	MethodSessionResume    = "session/resume"
	MethodSessionRead      = "session/read"
	MethodSessionSetModel  = "session/setModel"
	MethodSessionSetMode   = "session/setMode"
	MethodSessionUsage     = "session/usage"

	// Server -> client requests (ids `server-<n>`), which the client must answer.
	MethodSessionRequestRuntimePreferences           = "session/requestRuntimePreferences"
	MethodInteractionRequestPermission               = "interaction/requestPermission"
	MethodInteractionRequestUserInput                = "interaction/requestUserInput"
	MethodInteractionRequestProviderRuntimeHeaders   = "interaction/requestProviderRuntimeHeaders"
	MethodInteractionRequestOfficialMcpAuthHeaders   = "interaction/requestOfficialMcpAuthHeaders"
	MethodSubagentsReadRuntimeConfig                 = "subagents/readRuntimeConfig"
	MethodInteractionProviderRuntimeHeadersCancelled = "interaction/providerRuntimeHeadersCancelled"

	// Server -> client notifications.
	MethodStateUpdated              = "state.updated"
	MethodComputerUseOperationEvent = "computer-use/operation-event"
	MethodSessionEvent              = "session/event"
	MethodStartupStorageState       = "startup/storageState"
	MethodProcessResourceSample     = "process/resourceSample"
	MethodProcessMcpTelemetry       = "process/mcpTelemetry"
	MethodProcessMcpResourceSamples = "process/mcpResourceSamples"
	MethodProcessToolExecResource   = "process/toolExecResource"
	MethodProcessNetworkRequests    = "process/networkRequests"
	MethodPluginsOperationProgress  = "plugins/operationProgress"
)

// Provenance says how a method's shape was established.
const (
	ProvenanceProven = "PROVEN"
	ProvenanceStatic = "STATIC"
)

// Directions mirror codexproto's vocabulary so the two conformance tables read
// alike.
const (
	DirectionClientRequest      = "ClientRequest"
	DirectionServerRequest      = "ServerRequest"
	DirectionServerNotification = "ServerNotification"
)

// MethodInfo describes one method's direction, payload types and provenance.
type MethodInfo struct {
	Name      string
	Direction string
	// Params and Result name the Go types in this package, empty when none.
	Params string
	Result string
	// Provenance is ProvenanceProven or ProvenanceStatic (doc.go).
	Provenance string
	// Symbol is the minified bundle symbol the shape was read from.
	Symbol string
}

// Methods is every method the driver depends on.
var Methods = []MethodInfo{
	// Outbound requests.
	{Name: MethodSessionCreate, Direction: DirectionClientRequest, Params: "SessionCreateParams", Result: "Snapshot", Provenance: ProvenanceProven, Symbol: "qXt"},
	{Name: MethodSessionSend, Direction: DirectionClientRequest, Params: "SessionSendParams", Result: "SessionSendResult", Provenance: ProvenanceProven, Symbol: "QXt/Lhc"},
	{Name: MethodSessionSubscribe, Direction: DirectionClientRequest, Params: "SessionSubscribeParams", Result: "SessionSubscribeResult", Provenance: ProvenanceStatic, Symbol: "$Xt/_ci"},
	{Name: MethodSessionStop, Direction: DirectionClientRequest, Params: "SessionStopParams", Result: "", Provenance: ProvenanceStatic, Symbol: "rQt/wci"},
	{Name: MethodSessionResume, Direction: DirectionClientRequest, Params: "SessionResumeParams", Result: "Snapshot", Provenance: ProvenanceStatic, Symbol: "WXt/pci"},
	{Name: MethodSessionRead, Direction: DirectionClientRequest, Params: "SessionReadParams", Result: "Snapshot", Provenance: ProvenanceStatic, Symbol: "mci"},
	{Name: MethodSessionSetModel, Direction: DirectionClientRequest, Params: "SessionSetModelParams", Result: "Snapshot", Provenance: ProvenanceStatic, Symbol: "iQt/xci"},
	{Name: MethodSessionSetMode, Direction: DirectionClientRequest, Params: "SessionSetModeParams", Result: "Snapshot", Provenance: ProvenanceStatic, Symbol: "session/setMode"},
	{Name: MethodSessionUsage, Direction: DirectionClientRequest, Params: "SessionUsageParams", Result: "SessionUsageResult", Provenance: ProvenanceStatic, Symbol: "GYi"},

	// Server -> client requests.
	{Name: MethodSessionRequestRuntimePreferences, Direction: DirectionServerRequest, Params: "RequestRuntimePreferencesParams", Result: "RequestRuntimePreferencesResult", Provenance: ProvenanceProven, Symbol: "KYi/XXt"},
	{Name: MethodInteractionRequestPermission, Direction: DirectionServerRequest, Params: "RequestPermissionParams", Result: "PermissionResponse", Provenance: ProvenanceStatic, Symbol: "fXi/_F"},
	{Name: MethodInteractionRequestUserInput, Direction: DirectionServerRequest, Params: "RequestUserInputParams", Result: "UserInputResponse", Provenance: ProvenanceStatic, Symbol: "mXi/oDe"},
	{Name: MethodInteractionRequestProviderRuntimeHeaders, Direction: DirectionServerRequest, Params: "RequestProviderRuntimeHeadersParams", Result: "RequestProviderRuntimeHeadersResult", Provenance: ProvenanceStatic, Symbol: "hXi"},
	{Name: MethodInteractionRequestOfficialMcpAuthHeaders, Direction: DirectionServerRequest, Params: "RequestOfficialMcpAuthHeadersParams", Result: "", Provenance: ProvenanceStatic, Symbol: "_Xi/Koi"},
	{Name: MethodSubagentsReadRuntimeConfig, Direction: DirectionServerRequest, Params: "", Result: "SubagentRuntimeConfigResult", Provenance: ProvenanceStatic, Symbol: "Net"},

	// Server -> client notifications the timeline is built from.
	{Name: MethodStateUpdated, Direction: DirectionServerNotification, Params: "StateUpdatedParams", Result: "", Provenance: ProvenanceProven, Symbol: "Lhc/zFn/Xmc"},
	{Name: MethodComputerUseOperationEvent, Direction: DirectionServerNotification, Params: "OperationEventParams", Result: "", Provenance: ProvenanceStatic, Symbol: "Oai/dhc"},
	{Name: MethodSessionEvent, Direction: DirectionServerNotification, Params: "SessionEventParams", Result: "", Provenance: ProvenanceStatic, Symbol: "zXt/dhc"},
	{Name: MethodInteractionProviderRuntimeHeadersCancelled, Direction: DirectionServerNotification, Params: "", Result: "", Provenance: ProvenanceStatic, Symbol: "sT"},

	// Noise the driver drops. The first three are PROVEN only in the sense that
	// the canonical probe saw and filtered them (probe line 39); their payloads
	// were never read.
	{Name: MethodStartupStorageState, Direction: DirectionServerNotification, Params: "", Result: "", Provenance: ProvenanceProven, Symbol: "qNn"},
	{Name: MethodProcessResourceSample, Direction: DirectionServerNotification, Params: "", Result: "", Provenance: ProvenanceProven, Symbol: "sT"},
	{Name: MethodProcessMcpTelemetry, Direction: DirectionServerNotification, Params: "", Result: "", Provenance: ProvenanceProven, Symbol: "sT"},
	{Name: MethodProcessMcpResourceSamples, Direction: DirectionServerNotification, Params: "", Result: "", Provenance: ProvenanceStatic, Symbol: "sT"},
	{Name: MethodProcessToolExecResource, Direction: DirectionServerNotification, Params: "", Result: "", Provenance: ProvenanceStatic, Symbol: "sT"},
	{Name: MethodProcessNetworkRequests, Direction: DirectionServerNotification, Params: "", Result: "", Provenance: ProvenanceStatic, Symbol: "sT"},
	{Name: MethodPluginsOperationProgress, Direction: DirectionServerNotification, Params: "", Result: "", Provenance: ProvenanceStatic, Symbol: "sT"},
}

// Declares reports whether the method is in the catalog. Mirrors
// codexproto.Declares so the conformance tests read alike.
func Declares(method string) bool {
	for _, m := range Methods {
		if m.Name == method {
			return true
		}
	}
	return false
}

// Info returns the catalog row for a method.
func Info(method string) (MethodInfo, bool) {
	for _, m := range Methods {
		if m.Name == method {
			return m, true
		}
	}
	return MethodInfo{}, false
}
