// Package driver implements github.com/unidoc/spice.Driver on top of
// Fyne v2. It manages the framebuffer widget, translates Fyne key/mouse
// events into SPICE input messages, and wires clipboard traffic through
// the OS clipboard.
package driver

import (
	"image"
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"

	"github.com/unidoc/spice"
)

// fyneDo hops fn onto Fyne's main thread. From Fyne v2.7 the runtime
// enforces this — widget mutation from a random goroutine trips a
// warning and can crash the (non-reentrant) harfbuzz text shaper.
func fyneDo(fn func()) { fyne.Do(fn) }

// FyneDriver renders one SPICE display into a Fyne canvas and forwards
// input events back to the SPICE server. It is not tied to a specific
// window — the host app embeds Content() wherever it likes.
type FyneDriver struct {
	// Widgets
	display *canvas.Image // main framebuffer
	cursor  *canvas.Image // custom cursor overlay
	surface *inputSurface // catches mouse events + focus
	status  *widget.Label // bottom-line status
	content fyne.CanvasObject

	// OnReset, if set by the host application, is called when the user
	// clicks "Reset VM" in the console toolbar. Meant to hit the incus API
	// to hard-reset the VM. Kept as a callback so the driver package stays
	// independent of the incus package.
	OnReset func()

	// OnPasteRequest, if set, is called when the user clicks "Paste".
	// The host app reads the OS clipboard and calls PasteText on the
	// driver. Kept as a callback because the driver package deliberately
	// does not depend on fyne.Window or fyne.App just to read clipboard.
	OnPasteRequest func()

	// OnGuestClipboard is fired when the SPICE guest agent tells us the
	// guest has fresh text on its clipboard and we've fetched the bytes.
	// The host app should write it into the OS clipboard so the user can
	// paste it into Mac apps.
	OnGuestClipboard func(text string)

	// HostClipboard is called from the driver goroutine each time the
	// SPICE server asks "what do you have on the client clipboard?".
	// Host app returns current OS clipboard content. If nil, we tell the
	// server we have nothing.
	HostClipboard func() string

	// Local mirror of what we last wrote to the OS clipboard from the
	// guest. Used to break the feedback loop between the poll-based
	// change detector and the guest-driven SetContent — otherwise the
	// clipboard would keep bouncing back and forth forever.
	lastGuestClip string

	// Buffered modifier presses (Super/Alt) that haven't been sent to
	// the guest yet — see handleBufferedModifier. Guarded by mu.
	pendingMods []pendingModifier

	// Canvas we live inside — set by AttachToCanvas so toolbar button
	// callbacks can pull focus back to the surface without needing the
	// caller to reach in from outside every time.
	canvas fyne.Canvas

	// Display refresh coalescer. SPICE fires DisplayRefresh dozens of
	// times per second when the guest is scrolling or repainting; each
	// canvas.Image.Refresh triggers a full-frame texture upload (~4 MB
	// at 1280×800×32bpp), which is the bottleneck for scroll smoothness.
	// dirty is set on every SPICE update and drained by a single ticker
	// goroutine capped at ~30 fps — plenty for admin UI and a huge cut
	// in GPU/CPU vs unbounded refresh.
	dirty          atomic.Bool
	refreshStarted sync.Once

	// Set by the SPICE library on connect. Guarded by mu.
	mu    sync.Mutex
	inp   *spice.ChInputs
	main  *spice.ChMain
	frame image.Image

	// Cursor position last known (for MouseUp events, SPICE wants coords).
	lastX, lastY uint32
}

// New builds a driver that renders into its own widget hierarchy. Call
// Content() to get the fyne.CanvasObject to embed in your window.
func New() *FyneDriver {
	d := &FyneDriver{
		status: widget.NewLabel("Connecting..."),
	}
	d.surface = newInputSurface(d)

	// Toolbar: quick actions the user needs when they're stuck at BIOS
	// or an OS install prompt. Ctrl+Alt+Del is the classic Windows-in-VM
	// escape; Reset VM is the "netboot loop, get me out" button.
	// Every toolbar action refocuses the surface after firing so the
	// next keypress goes straight to the VM instead of the button that
	// was just clicked.
	cad := widget.NewButton("Ctrl+Alt+Del", func() {
		d.SendCtrlAltDel()
		d.focusSurface()
	})
	paste := widget.NewButton("Paste", func() {
		if d.OnPasteRequest != nil {
			d.OnPasteRequest()
		}
		d.focusSurface()
	})
	reset := widget.NewButton("Reset VM", func() {
		if d.OnReset != nil {
			d.OnReset()
		}
		d.focusSurface()
	})
	bottom := container.NewBorder(nil, nil,
		container.NewHBox(cad, paste, reset), // left-side buttons
		nil,                                  // right side empty
		d.status,                             // centre grows to fill
	)

	// DisplayInit() fills the surface once SPICE reports resolution.
	d.content = container.NewBorder(nil, bottom, nil, nil, d.surface)
	return d
}

// Content returns the top-level widget the caller should place in their
// window layout.
func (d *FyneDriver) Content() fyne.CanvasObject { return d.content }

// AttachToCanvas remembers which Fyne Canvas we're rendered into so the
// driver can pull focus back to the surface after any toolbar button
// click. Call once when the session's window is built, right after
// SetContent. Does NOT focus immediately — the caller should call
// RequestFocus() after the widget has been added to the visible tree,
// otherwise Fyne rejects the focus with "not part of canvas' content".
func (d *FyneDriver) AttachToCanvas(c fyne.Canvas) {
	d.mu.Lock()
	d.canvas = c
	d.mu.Unlock()
}

// RequestFocus pulls focus back to the surface using the previously
// attached Canvas. Call it when this session becomes the visible one
// in a multi-session app so key events go to the right VM.
func (d *FyneDriver) RequestFocus() { d.focusSurface() }

// focusSurface moves focus onto the input surface. Used internally by
// toolbar button callbacks; safe to call from any goroutine.
func (d *FyneDriver) focusSurface() {
	d.mu.Lock()
	c := d.canvas
	s := d.surface
	d.mu.Unlock()
	if c == nil || s == nil {
		return
	}
	fyneDo(func() { c.Focus(s) })
}

// SetStatus updates the label at the bottom of the console window.
// Called from any goroutine — safe.
func (d *FyneDriver) SetStatus(s string) {
	fyneDo(func() { d.status.SetText(s) })
}

// SendCtrlAltDel sends the three-key combo the way Windows expects it —
// each keydown in order, then all keyups. Guest OS sees the sequence
// atomically and triggers the security screen / login prompt / sign-out.
func (d *FyneDriver) SendCtrlAltDel() {
	d.mu.Lock()
	inp := d.inp
	d.mu.Unlock()
	if inp == nil {
		return
	}
	// Ctrl=0x1D, Alt=0x38, Del(extended)=0xE0 0x53
	inp.OnKeyDown([]byte{0x1D})
	inp.OnKeyDown([]byte{0x38})
	inp.OnKeyDown([]byte{0xE0, 0x53})
	inp.OnKeyUp([]byte{0xE0, 0x53})
	inp.OnKeyUp([]byte{0x38})
	inp.OnKeyUp([]byte{0x1D})
}

// PasteText types the given text as scancodes on the virtual keyboard.
// Works in every VM state, including pre-boot (BitLocker, WinRE, UEFI
// menus) where SPICE clipboard integration is not available because
// there is no guest agent running.
//
// The delay between keystrokes exists so a slow guest keyboard buffer
// (BitLocker especially) doesn't drop characters. Longer than needed
// hurts UX; shorter drops chars in the toughest cases.
func (d *FyneDriver) PasteText(text string) {
	d.mu.Lock()
	inp := d.inp
	d.mu.Unlock()
	if inp == nil {
		return
	}
	const perKeyDelay = 8 * time.Millisecond
	shiftDown := false
	setShift := func(want bool) {
		if want == shiftDown {
			return
		}
		if want {
			inp.OnKeyDown([]byte{0x2A}) // Left Shift
		} else {
			inp.OnKeyUp([]byte{0x2A})
		}
		shiftDown = want
	}
	// Count-only stats — the audit found that logging the actual
	// rune value here leaked clipboard content (BitLocker recovery
	// keys are 48 chars of digits + hyphens, well within paste-below-
	// threshold territory, and any non-US char in a passphrase used
	// to hit this branch). We now count-and-summarise instead.
	skipped := 0
	for _, r := range text {
		code, needShift, ok := runeToScancode(r)
		if !ok {
			skipped++
			continue
		}
		setShift(needShift)
		inp.OnKeyDown([]byte{code})
		inp.OnKeyUp([]byte{code})
		time.Sleep(perKeyDelay)
	}
	setShift(false) // Always release shift before returning.
	if skipped > 0 {
		log.Printf("incus-console: paste — %d chars had no US-scancode mapping (dropped)", skipped)
	}
}

// --- spice.Driver implementation ----------------------------------------

// DisplayInit is called once when the SPICE server negotiates the primary
// surface size. `img` is a live image whose pixels the SPICE library will
// keep mutating in place — we hand it to canvas.Image and call Refresh()
// on every DisplayRefresh to push it to the screen.
func (d *FyneDriver) DisplayInit(img image.Image) {
	d.mu.Lock()
	d.frame = img
	d.mu.Unlock()

	fyneDo(func() {
		d.display = canvas.NewImageFromImage(img)
		d.display.FillMode = canvas.ImageFillContain
		d.display.ScaleMode = canvas.ImageScaleFastest
		d.surface.setFrame(d.display)
		d.status.SetText("Connected")
	})
}

// DisplayRefresh is called by the SPICE library after any region of the
// framebuffer image has been mutated. Marks the display dirty; the
// coalescing ticker (see startRefreshLoop) drains dirty at ~30 fps and
// does the actual canvas.Image.Refresh() call. Doing a full refresh per
// SPICE update caused visible scroll lag because Fyne re-uploads the
// whole texture on each call.
func (d *FyneDriver) DisplayRefresh() {
	d.dirty.Store(true)
	d.refreshStarted.Do(d.startRefreshLoop)
}

// startRefreshLoop runs a single goroutine that consumes the dirty flag
// at a capped rate. First DisplayRefresh call kicks it off; it lives
// for the process lifetime (there's one driver per session and closing
// a session tears the whole driver down, so no leak).
//
// The default cap is ~30 fps — enough for smooth admin UI while
// halving the texture-upload cost of an uncapped SPICE refresh
// stream. Override via INCUS_CONSOLE_FPS (10–60). Over VPN, 20 is
// often indistinguishable from 30 for text/scroll workloads.
func (d *FyneDriver) startRefreshLoop() {
	fps := 30
	if v := os.Getenv("INCUS_CONSOLE_FPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 10 && n <= 60 {
			fps = n
		}
	}
	frameInterval := time.Second / time.Duration(fps)
	go func() {
		t := time.NewTicker(frameInterval)
		defer t.Stop()
		for range t.C {
			if !d.dirty.Swap(false) {
				continue
			}
			fyneDo(func() {
				if d.display != nil {
					d.display.Refresh()
				}
			})
		}
	}()
}

// SetEventsTarget stores the inputs channel — this is what mouse/keyboard
// handlers call into.
func (d *FyneDriver) SetEventsTarget(inp *spice.ChInputs) {
	d.mu.Lock()
	d.inp = inp
	d.mu.Unlock()
}

// SetMainTarget stores the main channel — used later for MonitorConfig etc.
func (d *FyneDriver) SetMainTarget(main *spice.ChMain) {
	d.mu.Lock()
	d.main = main
	d.mu.Unlock()
}

// SetCursor renders a client-side cursor overlay. `img` is the cursor
// bitmap, `x`,`y` are the hotspot offsets. Fyne doesn't expose a custom
// pointer at OS level so we overlay a small canvas.Image; the OS pointer
// stays visible too — acceptable for POC.
func (d *FyneDriver) SetCursor(img image.Image, x, y uint16) {
	if img == nil {
		return
	}
	fyneDo(func() {
		d.cursor = canvas.NewImageFromImage(img)
		d.cursor.FillMode = canvas.ImageFillOriginal
		// A production client would move this overlay to track mouse
		// position — good enough for POC to render it once so the shape
		// is at least present.
		d.surface.setCursor(d.cursor, x, y)
	})
}

// --- Clipboard integration --------------------------------------------
//
// Two directions:
//   guest → host: server calls ClipboardGrabbed (announcing "I have text"),
//                 we RequestClipboard to fetch bytes, then fire
//                 OnGuestClipboard so the host app can SetContent on the
//                 OS clipboard.
//   host → guest: the host app tells the driver "OS clipboard changed"
//                 via NotifyHostClipboardChanged. We SendGrabClipboard so
//                 the guest agent knows fresh content is available; when
//                 the guest pastes, the server calls ClipboardFetch and
//                 we return the current OS clipboard from HostClipboard.
//
// Text/UTF-8 only. Image clipboard is not implemented.

// ClipboardGrabbed fires when the guest copies. Fetch on our own
// goroutine — the SPICE main channel is on its ReadLoop and would
// deadlock if we called RequestClipboard synchronously here (Request
// blocks waiting for a message on the same connection we're being
// handled from).
func (d *FyneDriver) ClipboardGrabbed(sel spice.SpiceClipboardSelection, types []spice.SpiceClipboardFormat) {
	// Only bother if guest offers UTF-8 text. Everything else (images,
	// TIFF, etc) we'd have to plumb through separately.
	offersText := false
	for _, t := range types {
		if t == spice.VD_AGENT_CLIPBOARD_UTF8_TEXT {
			offersText = true
			break
		}
	}
	if !offersText {
		return
	}

	d.mu.Lock()
	main := d.main
	d.mu.Unlock()
	if main == nil {
		return
	}
	go func() {
		data, err := main.RequestClipboard(sel, spice.VD_AGENT_CLIPBOARD_UTF8_TEXT)
		if err != nil {
			log.Printf("incus-console: fetch guest clipboard: %v", err)
			return
		}
		text := string(data)
		d.mu.Lock()
		d.lastGuestClip = text
		cb := d.OnGuestClipboard
		d.mu.Unlock()
		if cb != nil {
			cb(text)
		}
	}()
}

// ClipboardFetch is called from ChMain's read loop when the guest agent
// asks for our clipboard content on paste. Must return promptly; we
// return whatever the host clipboard has right now via HostClipboard.
func (d *FyneDriver) ClipboardFetch(sel spice.SpiceClipboardSelection, format spice.SpiceClipboardFormat) ([]byte, error) {
	if format != spice.VD_AGENT_CLIPBOARD_UTF8_TEXT {
		return nil, nil
	}
	d.mu.Lock()
	cb := d.HostClipboard
	d.mu.Unlock()
	if cb == nil {
		return nil, nil
	}
	return []byte(cb()), nil
}

// ClipboardRelease — guest cleared its clipboard, no-op for us.
func (d *FyneDriver) ClipboardRelease(sel spice.SpiceClipboardSelection) {}

// NotifyHostClipboardChanged tells the guest agent that host clipboard
// content changed and it should pull fresh data on next paste. Called
// by the host clipboard poll loop when it detects a change. Returns
// silently if there is no active SPICE main channel.
func (d *FyneDriver) NotifyHostClipboardChanged() {
	d.mu.Lock()
	main := d.main
	d.mu.Unlock()
	if main == nil {
		return
	}
	if err := main.SendGrabClipboard(
		spice.VD_AGENT_CLIPBOARD_SELECTION_CLIPBOARD,
		[]spice.SpiceClipboardFormat{spice.VD_AGENT_CLIPBOARD_UTF8_TEXT},
	); err != nil {
		log.Printf("incus-console: notify guest of host clipboard: %v", err)
	}
}

// LastGuestClipboard returns the text we last received from the guest.
// Used by the host's clipboard poll loop to break the feedback loop:
// if the current OS clipboard content matches this, we know it came
// from the guest and we should NOT notify the guest of it as a change.
func (d *FyneDriver) LastGuestClipboard() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastGuestClip
}

// --- Input plumbing -----------------------------------------------------

// sendMouseMotion is called by the input surface on hover. Silent —
// motion events fire at ~60 Hz and would drown any real log line.
func (d *FyneDriver) sendMouseMotion(x, y int) {
	d.mu.Lock()
	inp := d.inp
	d.mu.Unlock()
	if inp == nil {
		return
	}
	d.lastX, d.lastY = uint32(x), uint32(y)
	inp.MousePosition(d.lastX, d.lastY)
}

// sendMouseButton forwards down/up. `button` is a SPICE button index:
// 1=left, 2=middle, 3=right, 4=up-wheel, 5=down-wheel.
func (d *FyneDriver) sendMouseButton(button uint8, down bool) {
	d.mu.Lock()
	inp := d.inp
	d.mu.Unlock()
	if inp == nil {
		return
	}
	if down {
		inp.MouseDown(button, d.lastX, d.lastY)
	} else {
		inp.MouseUp(button, d.lastX, d.lastY)
	}
}

// SendKey forwards a keyboard event. Exported so main.go can wire it into
// the window's desktop.Canvas key hooks — Fyne surfaces key events at the
// canvas level, not per-widget.
//
// Individual key events are NOT logged even at debug level, ever — a user
// typing a BitLocker recovery key or a login password would otherwise
// see it in the terminal.
//
// LeftSuper / RightSuper / LeftAlt / RightAlt are handled specially: we
// BUFFER the down event and only forward it when a non-modifier key
// follows (i.e. it's a real combo like Super+R). If the modifier goes
// up before any combo key, we discard the pair silently. That kills the
// common annoyance where an accidental Cmd tap on Mac fires the Windows
// Start menu, or a bare Alt opens the guest's menu bar.
//
// Shift and Ctrl are NOT buffered — those are commonly held for real UI
// gestures on their own (Shift for text selection, Ctrl for scrolling).
func (d *FyneDriver) SendKey(name fyne.KeyName, down bool) {
	d.mu.Lock()
	inp := d.inp
	d.mu.Unlock()
	if inp == nil {
		return
	}
	code := atScancode(name)
	if code == nil {
		if mod, ok := modifierScancodes[name]; ok {
			code = mod
		}
	}
	if code == nil {
		return
	}

	if isSuppressedModifier(name) {
		d.handleBufferedModifier(inp, name, code, down)
		return
	}

	// Non-modifier key event: flush any pending buffered modifiers
	// first so the guest sees them as part of the combo, then send the
	// key itself.
	if down {
		d.flushPendingModifiers(inp)
	}
	if down {
		inp.OnKeyDown(code)
	} else {
		inp.OnKeyUp(code)
	}
}

// isSuppressedModifier returns true for the modifier keys we don't want
// to leak on their own (they trigger Start menu / menu bar in Windows).
func isSuppressedModifier(name fyne.KeyName) bool {
	switch name {
	case desktop.KeySuperLeft, desktop.KeySuperRight,
		desktop.KeyAltLeft, desktop.KeyAltRight:
		return true
	}
	return false
}

// pendingModifier tracks one buffered modifier press we haven't sent yet.
type pendingModifier struct {
	name fyne.KeyName
	code []byte
}

func (d *FyneDriver) handleBufferedModifier(inp *spice.ChInputs, name fyne.KeyName, code []byte, down bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if down {
		// Only buffer if we don't already have this key buffered
		// (autorepeat can fire keydown many times before keyup).
		for _, p := range d.pendingMods {
			if p.name == name {
				return
			}
		}
		d.pendingMods = append(d.pendingMods, pendingModifier{name: name, code: code})
		return
	}
	// keyup: if we buffered the corresponding down without ever
	// releasing it, drop both silently.
	for i, p := range d.pendingMods {
		if p.name == name {
			d.pendingMods = append(d.pendingMods[:i], d.pendingMods[i+1:]...)
			return
		}
	}
	// Otherwise the down was already sent (real combo happened); send
	// the up so the guest doesn't see a stuck-down modifier.
	inp.OnKeyUp(code)
}

// flushPendingModifiers must be called BEFORE forwarding a non-modifier
// keydown, so the guest sees a modifier combo rather than a bare key.
// Buffered modifiers are then treated as "already down" — their eventual
// keyup goes straight through.
func (d *FyneDriver) flushPendingModifiers(inp *spice.ChInputs) {
	d.mu.Lock()
	pending := d.pendingMods
	d.pendingMods = nil
	d.mu.Unlock()
	for _, p := range pending {
		inp.OnKeyDown(p.code)
	}
}

// --- Input surface (custom widget) --------------------------------------

// inputSurface is the Fyne widget that hosts the framebuffer and captures
// mouse events. It implements desktop.Mouseable + desktop.Hoverable.
type inputSurface struct {
	widget.BaseWidget
	driver *FyneDriver
	frame  *canvas.Image
	cursor *canvas.Image
	curOff fyne.Position
}

func newInputSurface(d *FyneDriver) *inputSurface {
	s := &inputSurface{driver: d}
	s.ExtendBaseWidget(s)
	return s
}

func (s *inputSurface) setFrame(img *canvas.Image) {
	s.frame = img
	s.Refresh()
}

func (s *inputSurface) setCursor(img *canvas.Image, hotX, hotY uint16) {
	s.cursor = img
	s.curOff = fyne.NewPos(-float32(hotX), -float32(hotY))
	s.Refresh()
}

// CreateRenderer stacks frame + cursor overlay.
func (s *inputSurface) CreateRenderer() fyne.WidgetRenderer {
	return &surfaceRenderer{s: s}
}

// Ensure interface compliance at compile time — a typo in a method name
// would silently disable event delivery, so lock it down.
var _ desktop.Hoverable = (*inputSurface)(nil)
var _ desktop.Mouseable = (*inputSurface)(nil)
var _ fyne.Scrollable = (*inputSurface)(nil)
var _ fyne.Focusable = (*inputSurface)(nil)
var _ desktop.Keyable = (*inputSurface)(nil)
var _ fyne.Tabbable = (*inputSurface)(nil)

// --- Focus + keyboard on the surface widget ----------------------------
//
// Fyne routes key events to the focused widget FIRST — Canvas-level
// SetOnKeyDown/Up only fires when nothing is focused. That means as soon
// as the user clicks a button in the console window, all subsequent key
// presses stop reaching the VM. Making the surface itself Focusable +
// Keyable and pulling focus back after each button click keeps the VM
// receiving keystrokes without the user having to think about it.
//
// AcceptsTab returns true so Fyne doesn't hijack Tab for focus rotation
// (widget.Entry does the same trick).

func (s *inputSurface) FocusGained()              {}
func (s *inputSurface) FocusLost()                {}
func (s *inputSurface) TypedRune(_ rune)          {}
func (s *inputSurface) TypedKey(_ *fyne.KeyEvent) {}
func (s *inputSurface) AcceptsTab() bool          { return true }

// KeyDown forwards raw key events to the SPICE server via the driver.
// Same shape as the Canvas callback used to have, but this one is
// widget-level and receives keys reliably as long as we're focused.
func (s *inputSurface) KeyDown(e *fyne.KeyEvent) {
	s.driver.SendKey(e.Name, true)
}

func (s *inputSurface) KeyUp(e *fyne.KeyEvent) {
	s.driver.SendKey(e.Name, false)
}

// MouseIn/MouseOut — cursor entering/leaving the widget. No-op; we send
// motion events on every MouseMoved.
func (s *inputSurface) MouseIn(*desktop.MouseEvent) {}
func (s *inputSurface) MouseOut()                   {}
func (s *inputSurface) MouseMoved(e *desktop.MouseEvent) {
	x, y := s.frameCoords(e.Position)
	s.driver.sendMouseMotion(x, y)
}

// MouseDown / MouseUp map desktop.MouseEvent buttons → SPICE buttons.
func (s *inputSurface) MouseDown(e *desktop.MouseEvent) {
	if b := spiceButton(e.Button); b != 0 {
		s.driver.sendMouseButton(b, true)
	}
}
func (s *inputSurface) MouseUp(e *desktop.MouseEvent) {
	if b := spiceButton(e.Button); b != 0 {
		s.driver.sendMouseButton(b, false)
	}
}

// Scrolled implements fyne.Scrollable — trackpad two-finger scroll on the
// Mac, or wheel on external mouse. SPICE encodes scroll as click events
// on button 4 (up) and button 5 (down). Emit one click per notch;
// Fyne's ScrollEvent.Scrolled fires per notch already, so no accumulation
// logic needed for typical use.
func (s *inputSurface) Scrolled(e *fyne.ScrollEvent) {
	if e.Scrolled.DY == 0 {
		return
	}
	var btn uint8 = 4 // wheel up
	if e.Scrolled.DY < 0 {
		btn = 5 // wheel down
	}
	s.driver.sendMouseButton(btn, true)
	s.driver.sendMouseButton(btn, false)
}

func spiceButton(b desktop.MouseButton) uint8 {
	switch b {
	case desktop.MouseButtonPrimary:
		return 1
	case desktop.MouseButtonSecondary:
		return 3
	case desktop.MouseButtonTertiary:
		return 2
	}
	return 0
}

// frameCoords maps a widget-space position (Fyne uses DIP with float
// scaling) to framebuffer pixel coords. For POC we assume the display
// image fills the surface with `ImageFillContain` — dominant axis
// letterboxed. Refine later; for admin work, small offset is fine.
func (s *inputSurface) frameCoords(p fyne.Position) (int, int) {
	if s.frame == nil {
		return 0, 0
	}
	size := s.Size()
	img := s.frame.Image
	if img == nil {
		return int(p.X), int(p.Y)
	}
	iw := float32(img.Bounds().Dx())
	ih := float32(img.Bounds().Dy())
	// Fit-into-frame scale factor (ImageFillContain).
	scale := size.Width / iw
	if size.Height/ih < scale {
		scale = size.Height / ih
	}
	if scale <= 0 {
		return 0, 0
	}
	drawW := iw * scale
	drawH := ih * scale
	offX := (size.Width - drawW) / 2
	offY := (size.Height - drawH) / 2
	x := int((p.X - offX) / scale)
	y := int((p.Y - offY) / scale)
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x >= int(iw) {
		x = int(iw) - 1
	}
	if y >= int(ih) {
		y = int(ih) - 1
	}
	return x, y
}

// --- Renderer -----------------------------------------------------------

type surfaceRenderer struct {
	s *inputSurface
}

func (r *surfaceRenderer) Layout(size fyne.Size) {
	if r.s.frame != nil {
		r.s.frame.Resize(size)
	}
	if r.s.cursor != nil {
		r.s.cursor.Move(r.s.curOff)
	}
}
func (r *surfaceRenderer) MinSize() fyne.Size {
	return fyne.NewSize(640, 480)
}
func (r *surfaceRenderer) Refresh() {
	if r.s.frame != nil {
		r.s.frame.Refresh()
	}
}
func (r *surfaceRenderer) Objects() []fyne.CanvasObject {
	out := make([]fyne.CanvasObject, 0, 2)
	if r.s.frame != nil {
		out = append(out, r.s.frame)
	}
	if r.s.cursor != nil {
		out = append(out, r.s.cursor)
	}
	return out
}
func (r *surfaceRenderer) Destroy() {}
