# incus-console

Native cross-platform Go console client for
[incus](https://linuxcontainers.org/incus/). SPICE for VMs, `incus exec`
shell for containers. Runs on Linux, macOS, and Windows. Made by
[UniDoc](https://unidoc.io).

One window for everything: pick a host, drill down through projects to
instances, click any of them to open the right kind of console —
SPICE framebuffer for VMs, a full terminal for containers via `incus
exec` — with sessions kept alive as you switch between them. Built
because the SPICE clients that exist are painful, incus doesn't speak
VNC, and jumping between `incus console`, `incus exec`, and a separate
graphical viewer is a workflow tax.

Also a single-window read-only view of cluster members, networks, and
network ACLs across every project, without reaching for the `incus`
CLI.

## Features

- **Single window, sidebar tree** — Host → Project → Instance. Icons
  distinguish VMs (`🖥️`) from containers (`📦`). Live status updates
  every 15 s. Marker dots show which session is on screen (`●`), which
  are open in the background (`○`), and which are unopened.
- **SPICE framebuffer for VMs** — mouse, keyboard, wheel scroll, plus
  toolbar shortcuts for Ctrl+Alt+Del, Paste, and Reset VM. Modifier-alone
  taps (Cmd/Alt) are suppressed so accidental modifier taps don't fire
  Start menu or menu bar.
- **Full VT100 terminal for containers** — spawns bash (or sh if bash
  isn't there) via `incus exec` and wires it to a real terminal widget
  (colours, cursor addressing, arrow keys, ^C). vim, htop, less, tmux
  all work. `LS_COLORS` and a coloured `PS1` are set by default.
- **Bidirectional clipboard** via SPICE guest agent — guest copy →
  local paste and vice versa, when `spice-vdagent` is running in the
  guest. For pre-boot scenarios (BitLocker, UEFI menus) a **Paste**
  button types the OS clipboard as raw scancodes; confirms before
  pasting more than 100 characters.
- **Install incus-agent** flow — one-click Windows agent install. Given
  a cross-compiled binary plus the three per-VM cert files, incus-console
  serves them over an ephemeral HTTP listener. You paste a one-liner
  into the VM's PowerShell and the service is up in seconds.
- **Live session reconnect** — Reset VM keeps the window open, watches
  the boot sequence, and re-handshakes SPICE when the guest is back.
- **Restricted trust certs** — client cert scope is enforced server-side
  by incus; only accessible projects and instances appear in the tree.
- **Cluster and network observability** — read-only panels for cluster
  members, networks, and network ACLs, scoped per project, without
  switching to the `incus` CLI.

## Not included

- USB redirection — the underlying SPICE library doesn't ship it.
- Audio — supported by the library but gated off by default; flip the
  audiodev config in `raw.qemu.conf` to turn it on.
- Image clipboard — text/UTF-8 only.
- Multi-monitor — one framebuffer per session.

## Architecture

```
  Client (Linux/macOS/Windows)          incus host
┌────────────────────────────┐   HTTPS+WSS   ┌──────────┐
│ incus-console (Fyne)       │ ────────────► │ incusd   │
│  client-cert TLS auth      │               │  :8443   │
└──────────┬─────────────────┘               └────┬─────┘
           │                                      │
   VMs:    │ SPICE bytes over socketpair          │ Unix socket per QEMU SPICE server
   ├──────►│ tunneled through incus WSS proxies   │
   │       ▼                                      ▼
   │   unidoc/spice                           Guest VM (QEMU + SPICE)
   │
   Containers: exec WSS → PTY → text pane
                                                  ▼
                                              container process (bash/sh)
```

Every SPICE channel (main, display, cursor, inputs, clipboard, …) gets
its own WebSocket to the incus API. incus proxies each one to the QEMU
SPICE Unix socket via `ConsoleInstanceDynamic`. Container shell uses
`ExecInstance` with an interactive PTY over one WebSocket.

## Layout

```
cmd/incus-console/main.go         Entry: single window, tree sidebar, session map
internal/config/config.go         Config store (~/.config/incus-console/config.yaml)
internal/incus/connector.go       incus API client + spice.Connector + shell exec
internal/driver/driver.go         spice.Driver on Fyne (framebuffer + input)
internal/driver/keymap.go         Fyne key names → PC/AT scancodes, rune → scancode
internal/agent/agent.go           "Install incus-agent" HTTP installer server
internal/branding/                Embedded app icon
```

## Building

Requires Go 1.23+ and a C toolchain (Fyne pulls in a GL binding). The
runtime system libraries are the same across platforms — install the
equivalent package for yours:

**Linux**
```
sudo apt install libgl1-mesa-dev xorg-dev libasound2-dev \
  libopus-dev libopusfile-dev portaudio19-dev libpng-dev pkg-config
just build
```

**macOS**
```
brew install just opus opusfile portaudio libpng pkg-config
just build
```

**Windows (MSYS2 UCRT64 shell)**
```
pacman -S mingw-w64-ucrt-x86_64-{opus,opusfile,portaudio,libpng,pkgconf,go}
just build
```

Bare binary lands in `bin/incus-console` (or `.exe` on Windows). For a
signed macOS `.app` bundle: `just package-mac-native`.

`go.mod` uses local `replace` directives pointing at sibling checkouts
of `unidoc/spice` and `lxc/incus` — matches the workspace layout we
develop in. Set them to real module versions before publishing.

## First-time setup

1. Generate a client cert on your machine once:
   ```
   openssl req -x509 -newkey rsa:4096 -sha384 -days 3650 -nodes \
     -keyout client.key \
     -out   client.crt \
     -subj "/CN=your-name-laptop"
   ```
2. On each incus host, enroll the cert. See **Cert scoping** below to
   restrict it to specific projects instead of granting full admin.
3. In incus-console, click **Add…** and fill in the host name, URL
   (`https://host:8443`), and the paths to `client.crt` and `client.key`.
   Server cert is optional; pin it if the daemon runs behind a
   self-signed cert.

## Cert scoping

incus supports restricted trust certs so incus-console can be handed a
cert with least-privilege access rather than full admin. Two shapes:

**Admin cert (full access)** — reads and creates any project, any
instance. Fine for one-person teams; risky in shared environments.
```
incus config trust add-certificate --name alice-admin -- < client.crt
```

**Project-restricted cert** — cert can only see and touch the listed
projects. Cannot create new projects, cannot touch other tenants,
cannot admin cluster settings. This is the recommended shape for
per-operator certs in a shared cluster.
```
incus config trust add-token alice-unidoc \
  --projects unidoc \
  --restricted
# → outputs a base64 token
# Paste into `incus remote add unidoc-host https://host:8443` on your
# laptop — that enrolls the local client.crt as restricted.
# incus-console then uses the SAME cert with the same project scoping.
```

**Per-VM restriction** — incus does not offer per-instance ACLs on
trust certs. If you need "user X can console into VM A but not VM B in
the same project," put each VM in its own project and issue
project-scoped tokens. This is the incus-recommended pattern.

**Rotation and revocation:**
```
incus config trust list                              # see all trusts
incus config trust remove <fingerprint>              # revoke
```

incus-console picks up the new cert on next connect after a rotation.

## Secret storage

Client cert and key can each be supplied from one of three sources.
The Add / Edit Host dialog exposes them as radio-button tabs.

**File** — plaintext PEM paths on disk. Simplest, least secure; kept
for compatibility and for tmpfs-provisioned certs. Not recommended
for long-lived keys.

**System keychain** — PEMs stored in the OS-native secret store:

- macOS: Login Keychain / iCloud Keychain
- Linux: libsecret via D-Bus (Gnome Keyring, KWallet, KeePassXC)
- Windows: Credential Manager (DPAPI-encrypted)

Paste the PEM into the "System keychain" tab of the Host dialog once;
the plaintext is written to the OS keychain under service
`io.unidoc.incus-console` and never touches the incus-console config
file. On every connect the same OS API is used to read the PEM back.
Deleting the host from incus-console removes the keychain entries too.
No extra CLI or setup needed — pick this if you don't already have a
secrets manager.

**Command** — a shell command whose stdout is the PEM. The escape
hatch for advanced pipelines:

```yaml
hosts:
  - name: alpine-01
    url: https://alpine-01.internal:8443
    tls_client_cert_command: "op read op://Vault/alpine-01/client-cert"
    tls_client_key_command:  "op read op://Vault/alpine-01/client-key"
```

Any backend that exposes a CLI is supported:

- **1Password CLI** — `op read op://Vault/item/field`. The `op` agent
  handles biometric unlock, TTL, and audit logs.
- **sops + age** — `sops -d ~/secrets/alpine-01.key.enc`. Chain with
  an age key that lives in your SSH agent (`age-plugin-yubikey`,
  `sshcrypt`) so nothing sensitive is ever on-disk plaintext.
- **HashiCorp Vault** — `vault kv get -field=key secret/incus/alpine-01`.
- **YubiKey PKCS#11** — key never leaves the hardware chip; requires
  physical touch for each fetch.
- **Custom shell** — anything you can express as a pipeline.

The command is re-invoked on every new connection, so rotated
credentials are picked up without a restart. Stdout is held in memory
only, never written to disk or logged. Command stderr is included in
error dialogs (helpful for `op` / `sops` prompts) but stdout is never.

**Priority order** across sources is keychain > command > file, so a
host configured with both a keychain entry and a file path uses the
keychain and ignores the file.

## Windows guests

Windows install ISOs boot but can't see incus's virtio-scsi disks
without drivers. Use distrobuilder to repack the Windows ISO with
virtio drivers injected into `boot.wim` / `install.wim`:

```
distrobuilder repack-windows --windows-arch amd64 --windows-version w11 \
  Win11_original.iso Win11_incus.iso
```

Recommended incus VM config for Windows via `raw.qemu` and
`raw.qemu.conf`: disable secureboot, force `-cpu host`, replace
virtio-vga with plain VGA, remove virtio-audio, add usb-tablet, strip
the virtio-net PXE ROM.

## Known limitations

- **Latency** — SPICE was designed for LAN. Fine over a fast VPN; over
  public wifi mouse feels like mud. The point of this client isn't
  daily use anyway.
- **Cursor overlay** — the SPICE cursor bitmap is rendered but doesn't
  move with the pointer; the OS pointer stays visible.
- **Keyboard layout** — scancodes are US-layout. The guest OS handles
  layout translation; configure the guest for whatever layout you use.

## License

[Apache License 2.0](LICENSE.md). Made by [UniDoc](https://unidoc.io).
