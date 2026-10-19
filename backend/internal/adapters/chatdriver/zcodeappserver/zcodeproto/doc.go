// Package zcodeproto is the hand-written wire vocabulary of `zcode app-server
// --stdio`: method names, request/response/notification shapes, and the error
// codes the server uses.
//
// Hand-written because zcode ships no schema generator (Codex has
// `codex app-server generate-json-schema`; zcode has nothing comparable). The
// source of every shape is the minified desktop bundle, and each declaration
// here is tagged with the bundle symbol it was read from so a reader can go back
// to the evidence:
//
//   - PROVEN: exercised live by probes/zcode_final_probe.js and recorded in
//     HANDOFF.md ("App-server protocol contract"). Only session/create,
//     session/requestRuntimePreferences, session/send, state.updated and the three
//     noise notifications the probe filtered out (startup/storageState,
//     process/resourceSample, process/mcpTelemetry) have this status.
//   - STATIC: read from the bundle's zod schemas and handlers but never exercised.
//     A STATIC shape can be wrong in ways only a live exchange reveals; the Phase 1
//     probes in DESIGN-zcodeappserver.md section 6 promote rows to PROVEN.
//
// Provenance per method is recorded in Methods (methods.go). Every struct in
// types.go names its bundle symbol in its doc comment.
//
// Nothing here is a guess: a field that could not be read from the bundle is
// left out rather than invented, and behaviour that depends on an unobserved
// answer stays behind a seam in the driver (tagged with its U-number).
package zcodeproto

// ProviderVersion is the zcode build the bundle digest and every STATIC claim
// were read from (HANDOFF.md, `zcode --version`).
const ProviderVersion = "zcode 0.16.9"

// BundlePath is where the desktop app installs the CLI bundle every claim in
// this package was read from. The conformance test digests the file at this
// path when it exists and skips otherwise.
const BundlePath = "/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs"

// BundleSHA256 is `shasum -a 256` of BundlePath at the time the protocol was
// read. A bundle upgrade shows up as a digest mismatch, which is the cue to
// re-run the Phase 1 captures rather than trust the STATIC rows.
const BundleSHA256 = "31965ce1d5d4a808ef156784f67996de42ccab335591d2386a61ea8f6f506a54"
