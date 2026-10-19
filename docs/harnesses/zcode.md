# ZCode

AO supports ZCode (Z.AI's coding CLI) in TUI mode through the `zcode`
executable. ZCode runs as a one-shot `zcode -p` turn per spawn: the reply
streams in the session pane and the process exits, leaving AO's keep-alive
shell for inspection. There is no Chat driver yet — a `zcodeappserver` chat
driver over `zcode app-server --stdio` is the mapped follow-up.

## Install and launch

- ZCode ships bundled with the ZCode desktop app; there is no standalone
  installer, so AO's install page shows an explicit not-installable entry.
- AO resolves the `zcode` executable from `PATH` and common user-level
  locations. On app-bundle installs that is typically a wrapper script that
  exports the CLI's provider-config environment before exec'ing the bundled
  CLI — launching that wrapper (never a bare `zcode.cjs`) is required for the
  CLI to reach its provider.
- zcode 0.16.9 has no `--model`, `--session-id`, or `--append-system-prompt`
  flags. The effective model comes from `defaultModelSelection` in the CLI's
  personal provider config and is switchable per session with `/model`; AO
  therefore exposes no model field for zcode. Standing instructions are
  prepended to the `-p` prompt, which persists the role into the native
  session and survives `-c` restores.

## Credentials

AO owns an isolated credential store for zcode at
`<AO data dir>/harnesses/zcode` and injects it into every launch via
`ZCODE_DATA_BASE_DIR`. `Log in to ZCode` in settings runs `zcode login zai`
against that same store. An operator-set `ZCODE_DATA_BASE_DIR` in the daemon
environment wins on all three paths (probe, login, launch).

The raw z.ai token lives **one hour** and the CLI has no refresh flow, so AO's
auth probe reports `needs auth` once it expires — re-login before unattended
runs. Do not point `ZCODE_DATA_BASE_DIR` at the desktop app's shared
`~/.zcode/v2` store: the desktop writes its already-minted business token
there, which the CLI cannot re-exchange (`project_token_login_failed`).

## Permissions

zcode's `--mode` accepts build, edit, plan, yolo, or guarded, with a
documented default of yolo for `--prompt` runs (all approvals skipped).
Because of that, AO's default/unset mode is pinned to `--mode edit`;
accept-edits maps to edit, auto to build, bypass-permissions to yolo. The pin
is re-applied on restore. In headless `-p` runs, approval-class tool calls
fail fast ("No permission client configured") rather than hanging; workers
that need side-effecting shell commands must run bypass-permissions (yolo).

## Known limits

- `ao send` to a zcode TUI session is refused by the session manager with
  409 `HARNESS_ONE_SHOT`; the message names the harness and points at
  `ao spawn --agent zcode`. The launch puts nothing interactive in the pane:
  while `zcode -p` runs the text would go to its stdin, and after it exits
  (until the reaper's next 5 s tick flips the session to exited) the
  keep-alive shell would execute it. The refusal is per harness, so a
  restored session (`ao restore` / `ao resume-agent`, which relaunches as the
  interactive `zcode -c` TUI) is refused too; type into its pane directly. A
  terminated session keeps the usual 409 `SESSION_TERMINATED`. Chat-driver
  sessions, when they land, are relayed into their conversation and never
  gated.
- No workspace hooks in v1 (zcode hooks live in per-plugin `hooks.json`
  manifests), so the dashboard reads active work as idle; the reaper flips
  the session to exited within one tick of `zcode -p` returning.
- Restore is cwd-scoped `zcode -c` (latest session for the worktree); AO
  stores no native session id for zcode today.
