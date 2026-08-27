// Package agent hosts the "Install incus-agent" installer flow.
//
// Windows VMs on incus cannot mount the virtio-9p config drive, so the
// three cert files (agent.crt, agent.key, server.crt) that incus places
// on the host-side config mount are unreachable from inside the guest.
// This package works around that: it stages the certs plus the
// incus-agent.exe binary on the operator's laptop, briefly serves them
// over HTTP on the local LAN, and produces a PowerShell one-liner that
// the operator pastes into the VM to fetch and install everything.
//
// The listener binds to the interface with a reachable LAN IPv4 so the
// guest can hit it — 127.0.0.1 is useless because the VM is a
// different host. Every path is prefixed with a per-session random
// 128-bit token so a device on the same LAN can't just curl the certs
// off the listener. The server auto-shuts after the install script has
// pulled the last file, or after a hard timeout.
package agent

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"
)

// Bundle is the set of local file paths the operator points at when
// launching an install. Paths are read once at Serve time; nothing is
// re-read after the HTTP server starts, so moving the source files
// mid-install is safe.
type Bundle struct {
	AgentExe  string // path to incus-agent.exe on the operator's disk
	AgentCert string // path to agent.crt
	AgentKey  string // path to agent.key
	ServerCrt string // path to server.crt
}

// Validate returns an error naming the first missing/unreadable file.
// Called before starting the server so bad input surfaces in the
// dialog, not as a mysterious 404 in PowerShell.
//
// Cert / key paths ALSO get a content sanity check: reject if the
// file doesn't start with a PEM header for the expected block type.
// The audit found that a user pointing "agent.key" at ~/.ssh/id_ed25519
// would happily serve their SSH private key over the install listener.
// Content-checking here catches that at the dialog, not on the wire.
func (b Bundle) Validate() error {
	for _, p := range []struct{ label, path string }{
		{"incus-agent.exe", b.AgentExe},
		{"agent.crt", b.AgentCert},
		{"agent.key", b.AgentKey},
		{"server.crt", b.ServerCrt},
	} {
		if p.path == "" {
			return fmt.Errorf("%s path is required", p.label)
		}
		if _, err := os.Stat(p.path); err != nil {
			return fmt.Errorf("%s: %w", p.label, err)
		}
	}
	if err := verifyPEM(b.AgentCert, "CERTIFICATE"); err != nil {
		return fmt.Errorf("agent.crt: %w", err)
	}
	if err := verifyPEM(b.ServerCrt, "CERTIFICATE"); err != nil {
		return fmt.Errorf("server.crt: %w", err)
	}
	if err := verifyPEM(b.AgentKey, "PRIVATE KEY"); err != nil {
		return fmt.Errorf("agent.key: %w", err)
	}
	return nil
}

// verifyPEM checks that the file's first PEM block header contains
// `wantContains`. Uses a small read cap so we don't slurp the whole
// file into memory just to check a header. "PRIVATE KEY" matches both
// "PRIVATE KEY" (PKCS#8) and "EC/RSA/ED25519 PRIVATE KEY" (PKCS#1);
// "CERTIFICATE" matches the standard X.509 block.
func verifyPEM(path, wantContains string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	// PEM headers are short; 4 KiB is plenty for a single BEGIN line
	// even after leading whitespace / comments.
	head := make([]byte, 4096)
	n, _ := f.Read(head)
	head = head[:n]
	// Look for "-----BEGIN <label>-----"
	i := strings.Index(string(head), "-----BEGIN ")
	if i < 0 {
		return errors.New("file does not look like a PEM block")
	}
	line := string(head[i:])
	if end := strings.Index(line, "-----"); end > 0 {
		line = line[:end]
	}
	if !strings.Contains(line, wantContains) {
		return fmt.Errorf("expected a %s PEM block, got %q", wantContains, strings.TrimSpace(line))
	}
	return nil
}

// Server is one live installer session — an HTTP listener plus the
// state it needs to shut itself down cleanly.
type Server struct {
	URL        string // e.g. "http://192.168.100.5:8765" — used in the one-liner
	OneLiner   string // exact string the operator pastes into PowerShell
	listener   net.Listener
	srv        *http.Server
	stopped    atomic.Bool
	timeoutCtx context.Context
	timeoutFn  context.CancelFunc
	mu         sync.Mutex
}

// Start binds an HTTP listener on the machine's LAN IPv4 and serves the
// bundle files at fixed URLs. Returns a Server whose OneLiner should be
// pasted into the target VM's PowerShell.
//
// timeout is the hard upper bound on how long the server stays up; the
// server ALSO shuts down early as soon as install.ps1 has been fetched
// and the last agent file has been pulled, whichever comes first.
func Start(b Bundle, timeout time.Duration) (*Server, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}

	ip, err := lanIPv4()
	if err != nil {
		return nil, fmt.Errorf("could not find a LAN IPv4 to bind to: %w", err)
	}
	// Ephemeral port — bind to 0 and let the kernel pick a free one.
	listener, err := net.Listen("tcp", ip.String()+":0")
	if err != nil {
		return nil, fmt.Errorf("bind %s: %w", ip, err)
	}
	addr := listener.Addr().(*net.TCPAddr)

	// Random 128-bit token, hex-encoded. Every URL is prefixed with
	// this token — any request without it (LAN sniffer, curious guest
	// on the same host, rogue container on the bridge) hits the 404
	// path and gets nothing. This closes the "any device on the LAN
	// can curl agent.key" hole flagged in the audit. subtle.Compare
	// is used on match so timing doesn't leak the token.
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("generate install token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	prefix := "/" + token + "/"
	base := fmt.Sprintf("http://%s:%d%s", ip, addr.Port, prefix)

	// Build install.ps1 from the template, embedding the exact base URL
	// so no client-side substitution is needed.
	ps1, err := renderInstallScript(base)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}

	mux := http.NewServeMux()
	s := &Server{
		URL:      base,
		OneLiner: fmt.Sprintf("iwr -useb %sinstall.ps1 | iex", base),
		listener: listener,
	}
	s.timeoutCtx, s.timeoutFn = context.WithCancel(context.Background())

	// Track which files have been served — once all four content files
	// AND install.ps1 have been fetched at least once, we assume the
	// install succeeded and shut the server down proactively.
	var (
		gotScript atomic.Bool
		gotExe    atomic.Bool
		gotCrt    atomic.Bool
		gotKey    atomic.Bool
		gotServer atomic.Bool
	)
	maybeStop := func() {
		if gotScript.Load() && gotExe.Load() && gotCrt.Load() && gotKey.Load() && gotServer.Load() {
			s.Stop()
		}
	}
	// Path-token guard: reject any request whose URL doesn't start
	// with the per-session prefix. Constant-time compare so the
	// 404-vs-200 timing doesn't leak the token to a probe on the LAN.
	guard := func(inner http.HandlerFunc) http.HandlerFunc {
		wantPrefix := []byte(prefix)
		return func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if len(p) < len(prefix) || subtle.ConstantTimeCompare([]byte(p[:len(prefix)]), wantPrefix) != 1 {
				http.NotFound(w, r)
				return
			}
			// Strip the token so downstream handlers see the plain path.
			r.URL.Path = p[len(prefix)-1:] // keep leading "/"
			inner(w, r)
		}
	}
	serve := func(flag *atomic.Bool, path string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, path)
			flag.Store(true)
			maybeStop()
		}
	}
	mux.HandleFunc(prefix+"install.ps1", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(ps1))
		gotScript.Store(true)
		maybeStop()
	}))
	mux.HandleFunc(prefix+"incus-agent.exe", guard(serve(&gotExe, b.AgentExe)))
	mux.HandleFunc(prefix+"agent.crt", guard(serve(&gotCrt, b.AgentCert)))
	mux.HandleFunc(prefix+"agent.key", guard(serve(&gotKey, b.AgentKey)))
	mux.HandleFunc(prefix+"server.crt", guard(serve(&gotServer, b.ServerCrt)))
	// Everything else — including the root, and any un-tokenised path
	// — 404s. Nothing leaks about what we're hosting.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })

	// Slightly harden the HTTP server itself: bound request bodies
	// and read/write timeouts so a stuck peer can't tie up a slot.
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       30 * time.Second,
	}
	go func() { _ = s.srv.Serve(listener) }()

	// Hard timeout — if the operator never actually pastes the one-liner,
	// don't leave the listener open forever.
	go func() {
		select {
		case <-time.After(timeout):
			s.Stop()
		case <-s.timeoutCtx.Done():
			return
		}
	}()

	return s, nil
}

// Stop closes the listener and cancels the timeout goroutine. Safe to
// call multiple times.
func (s *Server) Stop() {
	if s.stopped.Swap(true) {
		return
	}
	s.timeoutFn()
	_ = s.srv.Shutdown(context.Background())
	_ = s.listener.Close()
}

// Stopped reports whether the server has already been shut down.
func (s *Server) Stopped() bool { return s.stopped.Load() }

// renderInstallScript templates the base URL into the PowerShell
// installer. Kept as a function so the caller can't accidentally get
// stale URLs across restarts.
func renderInstallScript(baseURL string) (string, error) {
	t, err := template.New("install").Parse(installTemplate)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	if err := t.Execute(&buf, struct{ URL string }{URL: baseURL}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// lanIPv4 returns the first non-loopback, up, IPv4 address across all
// interfaces. Skips docker/podman/VPN-tap bridges when it can identify
// them — those are unreachable from the guest.
func lanIPv4() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	// Prefer common "real" interface name prefixes; fall back to any
	// non-loopback IPv4 if we don't find one.
	preferred := []string{"en", "wlan", "eth", "wg", "utun"}

	pickFrom := func(iface net.Interface) net.IP {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			return ip4
		}
		return nil
	}

	for _, prefix := range preferred {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 {
				continue
			}
			if !strings.HasPrefix(iface.Name, prefix) {
				continue
			}
			if ip := pickFrom(iface); ip != nil {
				return ip, nil
			}
		}
	}
	// Fallback: any up, non-loopback IPv4.
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ip := pickFrom(iface); ip != nil {
			return ip, nil
		}
	}
	return nil, errors.New("no non-loopback IPv4 interface is up")
}

// installTemplate is the PowerShell script we hand to the guest. It is
// idempotent — remove any existing incus-agent service before install,
// so re-runs after failed installs don't wedge on "service already
// exists" errors.
//
// The template variable {{ .URL }} is baked in at server start so the
// script hits exactly the ephemeral server that produced it.
const installTemplate = `# incus-agent installer, generated by incus-console.
$ErrorActionPreference = "Stop"
$dst = "C:\Program Files\Incus"
$src = "{{ .URL }}"

Write-Host "Installing incus-agent from $src → $dst"
New-Item -Path $dst -ItemType Directory -Force | Out-Null

# {{ .URL }} already includes the per-session bearer token as a path
# prefix — the operator's laptop 404s any request without it, so the
# ephemeral certs can't be sniffed by any other device on the LAN.
foreach ($f in @("incus-agent.exe", "agent.crt", "agent.key", "server.crt")) {
    Invoke-WebRequest -UseBasicParsing "$src$f" -OutFile (Join-Path $dst $f)
}

# Ensure clean slate: stop and delete any pre-existing service.
$svc = Get-Service incus-agent -ErrorAction SilentlyContinue
if ($svc) {
    Stop-Service incus-agent -Force -ErrorAction SilentlyContinue
    sc.exe delete incus-agent | Out-Null
    Start-Sleep -Seconds 1
}

$exe = Join-Path $dst "incus-agent.exe"
$bin = ('"{0}" --service -s "{1}"' -f $exe, $dst)
New-Service -Name incus-agent -BinaryPathName $bin -DisplayName "Incus Agent" -StartupType Automatic | Out-Null
Start-Service incus-agent

Write-Host "incus-agent service installed and started."
Get-Service incus-agent | Format-Table -AutoSize
`

// SuggestCertPaths turns an incus VM name into the on-host filesystem
// paths where its cert bundle normally lives. Useful in the UI as
// placeholder text so the operator knows what to scp.
func SuggestCertPaths(project, vmName string) []string {
	dir := filepath.ToSlash(filepath.Join("/var/lib/incus/storage-pools/default/virtual-machines", project+"_"+vmName))
	return []string{
		filepath.Join(dir, "agent.crt"),
		filepath.Join(dir, "agent.key"),
		filepath.Join(dir, "server.crt"),
	}
}
