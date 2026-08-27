# incus-console — repository notes for Claude

This file scopes Claude's behaviour on the `incus-console` codebase.
Load-bearing conventions and non-obvious invariants live here so future
sessions catch them without re-deriving from the code.

## What this project is

A native cross-platform Go client for [incus](https://linuxcontainers.org/incus/).
SPICE framebuffer for VMs, `incus exec` shell for containers. Wrapped in
a single-window Fyne UI. Ships as an `.app` bundle (macOS), an ELF/AppImage
(Linux) and an `.exe` (Windows). Apache 2.0 under UniDoc branding.

It is **not** a full incus GUI — the north star is "get me into the
console when RDP/SSH is broken". Cluster / network / ACL / storage views
are read-only observability only; mutation of those objects is out of
scope and belongs in the `incus` CLI or automation.

## Build / test / lint

`just` orchestrates everything (see `Justfile` for the full list):

- `just tidy` — first run on a fresh clone. Resolves `go.mod`.
- `just vendor` — populates `./vendor`. Required before `just build`,
  and required before every fyne-cross packaging step (the Docker
  container can't reach the sibling `../spice` clone).
- `just build` — native build → `./bin/incus-console`. On Linux you need
  Fyne's system libs first: `libgl1-mesa-dev xorg-dev libasound2-dev
  libopus-dev portaudio19-dev libpng-dev`. On macOS: `brew install
  opus portaudio libpng`.
- `just run` — `go run` the CLI.
- `just vet` — `go vet` OUR code (`GOOS=darwin` so darwin-only paths
  still get checked from Linux). Vendored Fyne/audio deps fail to vet
  on Linux without X11/portaudio headers — that's not a bug.
- `just fmt` — `gofmt -s` + `goimports`.
- `just test` — `go test ./...`.
- `just package-mac-arm` — cross-compile the arm64 `.app` from Linux
  via fyne-cross + osxcross. Requires `MACOSX_SDK_PATH` pointing at a
  Mac SDK tarball you extracted yourself.

## Layout

```
cmd/incus-console/          main.go — single-window Fyne UI
internal/
  agent/                    Windows install-agent HTTP flow + fetch.go
  branding/                 icon + custom Fyne theme
  config/                   YAML config @ ~/.config/incus-console/config.yaml
  creds/                    keychain / command / file PEM source abstraction
  driver/                   spice.Driver impl on top of Fyne
  incus/                    incus client wrapper (SPICE, exec, whoami, ...)
vendor/                     go mod vendor output — NOT in git (see below)
patches/                    (does not exist — do not create it)
```

## Non-obvious invariants — HONOUR THESE

Security is the north star for this tool. The rules below are load-bearing.

- **Private key bytes never touch disk in plaintext.** The `creds`
  package supports three sources — keychain, command stdout, file path
  — in that priority order. Keychain uses the OS-native store (macOS
  Security.framework, libsecret, Windows Credential Manager). The
  file-path option is a deliberate opt-in for compatibility, not the
  recommended path. Command stdout is held in memory only, never
  cached, and never logged.
- **Command stderr from credential sources is scrubbed** —
  `creds.Load` truncates to the first line and 80 bytes before
  surfacing to the error dialog. Do not remove the truncation: tools
  like `sops`, `op`, `vault`, `security` sometimes emit partial secret
  material (envelope headers, cached tokens) on decrypt failure.
- **Never log clipboard content.** Only counts. The paste driver used
  to log unmappable runes individually — that leaked BitLocker keys
  and passphrases one char at a time. Search for `PasteText` in
  `internal/driver/driver.go` before touching it.
- **Paste-confirm threshold is 24 chars**, low enough to catch typical
  secrets. Do NOT raise it without asking.
- **Install-agent HTTP listener uses a per-session random 128-bit
  token as a path prefix.** Every request without that token 404s.
  The listener also binds to a LAN IPv4 (not 127.0.0.1 because the
  guest VM is a different host) and enforces read/write/idle timeouts.
  `agent.Bundle.Validate` also PEM-header-checks each cert/key file so
  a user pointing "agent.key" at `~/.ssh/id_ed25519` gets rejected at
  the dialog, not at the wire.
- **fsnotify config auto-reload asks before applying command-source
  changes.** `main.go`'s `suspiciousCommandDiff` flags any `_command`
  field mutation between the running config and a fresh disk load,
  and pops a confirm dialog before the new config becomes live —
  otherwise the watcher itself is an RCE channel.
- **Config file writes are atomic** (temp-file + rename in same dir).
  A crash mid-write leaves the previous config intact.
- **Reset VM, Force stop, Restart** all require confirm dialogs that
  name the specific host + project + instance. Do not bypass.
- **Restricted trust certs** should ideally hide the action buttons
  the token can't run — but that is not yet done (see the audit
  backlog).
- **SPICE Debug logging is gated behind `INCUS_CONSOLE_SPICE_DEBUG=1`.**
  Session IDs, agent capabilities and clipboard sizes leak to stderr
  otherwise.

## Fyne v2.7 threading rules

Fyne v2.7 enforces main-thread widget mutation. Any goroutine touching
a widget MUST go through `fyneDo(func() { ... })` (defined in `main.go`
and `internal/driver/driver.go`) which routes to `fyne.Do`. Calling
widget methods directly from a goroutine will either warn or crash the
harfbuzz text shaper. Search for `fyneDo` before adding new async UI
paths.

`fyne.DoAndWait` blocks until the fn runs on main — use it only when
you need a return value (see `HostClipboard` in `main.go`). `fyne.Do`
is fire-and-forget.

## Vendored dependency management

`./vendor` is `.gitignore`d because it's ~54 MB. **Regenerate with
`just vendor` on any fresh clone.**

Do NOT hand-edit anything under `./vendor`. `go mod vendor` will
overwrite your change on the next refresh. If a vendored dependency
needs a real fix, do one of:

1. Fork it upstream (see the `spice` sibling for the pattern: fork
   the upstream module, rename its `module` line, then add a `replace`
   directive in `go.mod` pointing at the fork).
2. Send a PR to upstream and pin the module version to a fork branch.
3. Live with the upstream behaviour.

## The `spice` sibling clone

`go.mod` has `replace github.com/unidoc/spice => ../spice`. The
sibling `../spice` is a local mirror of the unidoc/spice fork
(see [feedback_never_commit_to_local_forks](../../.claude/projects/-home-tower-workspace/memory/feedback_never_commit_to_local_forks.md)).

**Never `git commit` inside `../spice`.** Changes to the spice library
land as patch files under `../spice/temp/` (produced with
`git format-patch origin/master -o temp/`). The user pushes them from
elsewhere. If a session needs the spice code changed, edit files there,
generate patches into `temp/`, then `git reset --hard origin/master`
so the local mirror stays clean.

## Style

Standard Go — `gofmt -s`, `goimports`, no comment on obvious code,
comments explain **why** not **what**. Doc-strings on exported symbols.
Use `%w` when wrapping errors. Never re-export types with the sole
purpose of decoupling; the `internal/` layout already does that.

Icons on state-changing UI use `theme.Media*Icon()` where possible so
the button intent is legible without reading the label.

## Testing manually

Rebuild + smoke test path (Mac):

```bash
cd ~/incus-console && just vendor && just build
./bin/incus-console
```

For UI changes: open at least one host, click into an instance, verify
Open console / Start / Stop / Restart / Force stop / Reset all behave.
For SPICE-level changes: `INCUS_CONSOLE_SPICE_DEBUG=1 ./bin/incus-console`
and tail the debug output during a session open.

## What is out of scope

- Full incus object management (networks, ACLs, storage volumes,
  profiles). Read-only views only, per the north star.
- USB redirection (SPICE library upstream doesn't support it).
- Multi-monitor SPICE (one framebuffer per session).
- Image/binary clipboard (text only).
- Custom keyboard layouts (SPICE scancodes are US; guest OS translates).
