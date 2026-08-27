# incus-console — TODO

Backlog from audits + performance review. Ranked by impact and grouped
by where the work lives. Anything without an owner is up for grabs.

## Performance — done this pass

- [x] SPICE frame-buffer sync.Pool (5 size classes) — `../spice/conn.go`
- [x] MOUSE_POSITION coalescing at 60 fps ceiling — `../spice/ch-inputs.go`
- [x] Configurable driver FPS cap via `INCUS_CONSOLE_FPS` (default 30) — `internal/driver/driver.go`
- [x] Trust-scope badge (admin / restricted) in window title — `main.go` `scopeBadge`
- [x] `reverseImgRGBA` in-place vertical flip (~8 MB alloc / frame saved) — `../spice/image.go`

## Performance — bigger wins pending

- [ ] **SPICE image cache** (CACHE_ME / FROM_CACHE flags). Server tags
      reusable icons/UI chunks — we currently ignore the flag and
      re-decode every frame. LRU on decoded `image.RGBA` keyed by
      `SPICE_IMAGE_ID`. Direct bandwidth save on repeat renders.
- [ ] **LZ4 image type support** (`SPICE_IMAGE_TYPE_LZ4`, 109). ~3×
      faster than LZ and better compressed. Needs a `lz4` decoder
      dep in `image.go`. Bandwidth + CPU win.
- [ ] **Video streaming path** (`SPICE_MSG_DISPLAY_STREAM_*`). Server
      MJPEGs regions flagged as "video"; huge for browsing YouTube or
      any animated content in a VM. Requires QEMU `stream-video=all`
      + guest QXL driver, plus stream-msg handlers in
      `ch-display.go`. Biggest single win for interactive workloads.
- [ ] **Dirty-rect texture upload in Fyne**. Currently
      `canvas.Image.Refresh()` uploads the full framebuffer on every
      tick (~7 MB @ 1080p RGBA). SPICE already knows the exact
      changed rectangles; we drop that info. Would need a custom
      Fyne widget or upstream feature. High effort, biggest CPU win.
- [ ] **Frame allocator hits for cursor / draw commands** — the pool
      today only covers `ReadData`. `SetCursor`, `Draw*` decoders
      still `make` on the hot path.
- [ ] **Debug logging routed through `client.Debug`** for the
      remaining hardcoded `log.Printf` calls in the spice library
      (ch-main.go especially). Not perf, but sensitive-ish info
      leaks to stderr on every session — see the security audit.

## SPICE protocol gaps (functionality)

- [ ] Cursor decoder handles only ALPHA. Add MONO / COLOR16 / COLOR24
      / COLOR32 support in `ch-cursor.go decodeCursor`.
- [ ] LZ palette formats (`LZ_IMAGE_TYPE_PLT1_LE/BE`,
      `PLT4_LE/BE`) — currently return `errors.New("TODO ...")`.
      Older Windows guests (XP-era themes) render blank without them.
- [ ] `SPICE_MSG_CURSOR_INVAL_ALL` — clear cache TODO in
      `ch-cursor.go` handler.
- [ ] `SPICE_MSG_WAIT_FOR_CHANNELS` — logged and ignored in
      `conn.go`. Investigate if the server expects a specific reply.
- [ ] USB redirection — channel type acknowledged, no impl. Big
      undertaking (needs libusb or platform-specific device access).
- [ ] TLS support on direct SPICE connections (i.e. not the
      incus-WSS tunnel we use today). Third-party embedders who
      connect straight to `spice://host:port/` currently have no way
      to enable TLS. `Connector` returning a `*tls.Conn` already
      works; needs docs + optional helper. Keep the plaintext-auth
      fallback callable when the transport is already authenticated.

## Security follow-ups from the audit

- [ ] **Certificate fingerprint pinning** (SPICE-level). Prevents
      MITM even inside a TLS tunnel where an active attacker could
      substitute the SPICE-level RSA pubkey. Requires a Config field.
- [ ] **Log file discipline** — write to
      `~/.config/incus-console/logs/incus-console.log` with rotation,
      document what's logged, ensure secret material stays out.
- [ ] **Restricted-scope action hint** — Restricted trust doesn't
      mean "can't Stop/Start"; it means "scoped to specific
      projects". Improve error UX when an action fails with 403
      (recognisable message, link to incus docs on trust scoping).
      Do NOT blanket-hide state buttons.
- [ ] **First-use approval for command-source credentials**. Any
      change to `TLSClientCertCommand` / `TLSClientKeyCommand`
      should prompt for approval keyed on the command's SHA-256
      (stored in preferences). Complements the existing fsnotify
      auto-reload gate.
- [ ] **PEM entry masking** in host form — currently
      `NewMultiLineEntry` shows the private key on-screen. Add a
      "show / hide" toggle button.

## UX polish

- [ ] **Progress bar for agent binary Fetch** button. Currently
      flips label to "Downloading…" with no % — a stalled fetch and
      an in-flight one look identical. Use `Content-Length` from
      the HTTP response.
- [ ] **First-run affordance** — empty-state page currently reads
      "Pick a VM from the sidebar" but the sidebar is empty. Add a
      big "Add your first host" button on the landing page.
- [ ] **Legend / tooltip** for the sidebar state markers (● / ○ /
      blank). Undocumented today.
- [ ] **Config reload toast** — the auto-reload path already fires
      an info dialog, but a less-intrusive toast would fit better
      for the "background thing happened" pattern.
- [ ] **Right-click VM in the tree** → Start / Stop / Reset shortcut
      menu. Info panel already exposes the same actions, but a
      right-click saves a click for frequent users.
- [ ] **Live SPICE serial console log stream** for netboot / kernel
      panic debugging — read from the console channel instead of
      SPICE display so we work even when framebuffer is broken.
- [ ] **Delete-host dialog wording** on command-source hosts —
      currently says "credentials will be deleted too" but only
      keychain accounts are wiped. Command targets (1Password,
      Vault) stay put; state that.
- [ ] **Guest → host clipboard cap** — big pastes into the host OS
      clipboard should confirm above a threshold, like the reverse
      direction already does.

## Coverage

- [ ] **SPICE unit tests** — near-zero today outside `quic/`. Table-
      driven golden-bytes tests for cursor, main, image, lz
      decoders would let us refactor with confidence.
- [ ] **incus-console UI smoke test harness** — even a `just test`
      that spins up a fake incus and does add-host → open-console
      → close-session would catch the regressions this audit
      surfaced by hand.

## VPN-specific tunings (env / config, not code)

Documented in CLAUDE.md so operators can try them:

- Wireguard MTU: `wg set wg0 mtu 1280` fixes mysterious hiccups on
  paths with lower path MTU.
- QEMU: `streaming-video=filter` limits the video-stream heuristic
  to genuinely-moving regions.
- Guest SPICE tools: install the qxl driver + spice-vdagent for
  best bandwidth savings + clipboard/mouse sync.
