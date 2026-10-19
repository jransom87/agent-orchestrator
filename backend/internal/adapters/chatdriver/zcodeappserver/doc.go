// Package zcodeappserver drives ZCode through `zcode app-server --stdio` and
// normalizes its notifications into AO's provider-neutral Chat events. It is
// the persistent conversational adapter for the zcode harness; the one-shot
// `-p` TUI adapter in adapters/agent/zcode stays as it is.
//
// Transport (DESIGN-zcodeappserver.md 3.3). The wire is NDJSON validated by
// the server against a strict 4-way union: request {id, method, params?},
// response {id, result|error}, notification {method, params?}, and server->client
// request {id:"server-<n>", method, params}. There is no "jsonrpc" field, so
// codex's rpc.go is reused with two deltas: protocol-level rejections
// ({id:"parse-error"} / {id:"invalid-message", error.data.issues}) are logged
// at Error because they mean AO wrote a frame the strict schema refused, and a
// handler may choose its own refusal code (-32601 for anything AO does not
// model, the only code that lets session/requestRuntimePreferences fall back
// to defaults). Our ids are an int64 counter seeded from the persistent host on
// reconnect; the server re-announces an unanswered interaction request under a
// fresh server-<n> id every 1-10 s and every one of those ids is answered.
//
// Deferred turns (3.6). The session/send handler emits
// state.updated{reason:"prompt_started"} synchronously BEFORE it returns the
// {accepted} result, and the ack carries no turn id. SendTurn therefore only
// mints the ProviderTurnID; the controller binds it and calls StartDeferredTurn,
// which arms the turn tracker and only then writes session/send. Any other
// ordering lets the first notification reach the pump with no pending id.
//
// Standing instructions (4.3). zcode has no system-prompt channel on
// session/create or session/send. The ACP preamble pattern is used: the role
// text is prepended to the first prompt that actually reaches the provider and
// skipped on a resume that already has turns, byte-for-byte what the TUI
// adapter does for -p, so a -c restore sees the same first turn.
//
// Provenance. Everything in zcodeproto is tagged PROVEN (exercised by the
// canonical probe) or STATIC (read from the minified bundle). The driver encodes
// the STATIC answers and leaves every UNKNOWN (U-numbered in the design) behind
// a seam: the runtimeHeadersResolver for U0, the message-prefix branch on -32004
// for U7, the text-source latch for U2. Nothing here guesses.
//
// Credentials. The zcode token is a raw one-hour OAuth JWT. This package never
// reads, logs, prints or embeds it: AuthStatus is consulted through the agent
// plugin, the credential store is pinned through AugmentRuntimeLaunchEnv, and
// stderr (where the CLI's token logger writes) is discarded.
//
// Phase 0 ships the package without registering it in registry.Build: a
// registered driver whose Probe succeeds changes spawn behaviour, and the
// capability floor is unproven until the Phase 1 captures land.
package zcodeappserver
