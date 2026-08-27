// Package incus opens a SPICE console session on an incus VM and exposes it
// as a spice.Connector — every call to SpiceConnect() opens a new tunneled
// connection to the same QEMU SPICE socket.
//
// Under the hood we call ConsoleInstanceDynamic on the incus REST API, which
// returns a factory function `func(io.ReadWriteCloser) error` — each call
// spawns an incus-side proxy that mirrors bytes between a fresh WebSocket
// (talking to QEMU) and the passed pipe. We hand it one end of an in-memory
// pipe and return the other end to the SPICE library. From the library's
// perspective it's just a net.Conn.
package incus

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"

	"github.com/unidoc/incus-console/internal/creds"
)

// Config identifies one incus host and the client credentials to
// authenticate with. It's a lightweight passthrough of the fields we
// need from config.Host so this package doesn't depend on the config
// schema.
type Config struct {
	URL     string
	Project string // empty = default

	// ClientCert and ClientKey each pick a source in the creds package
	// (keychain > command > file path). See internal/creds for the
	// priority rules.
	ClientCert creds.Source
	ClientKey  creds.Source

	TLSServerCert string // path to PEM, optional; empty = system CA
}

// Server wraps the incus.InstanceServer with the small subset of behaviour
// incus-console needs. Keeping it named locally lets the GUI code depend on this
// package alone.
type Server struct {
	Client incus.InstanceServer
}

// Connect authenticates against the incus daemon with a TLS client cert.
// Set InsecureSkipVerify=false: the server cert MUST either come from a real
// CA, or be pinned via cfg.TLSServerCert.
func Connect(ctx context.Context, cfg Config) (*Server, error) {
	cert, err := creds.Load(ctx, "client cert", cfg.ClientCert)
	if err != nil {
		return nil, err
	}
	key, err := creds.Load(ctx, "client key", cfg.ClientKey)
	if err != nil {
		return nil, err
	}
	var serverCert string
	if cfg.TLSServerCert != "" {
		buf, err := os.ReadFile(cfg.TLSServerCert)
		if err != nil {
			return nil, fmt.Errorf("read server cert: %w", err)
		}
		serverCert = string(buf)
	}

	args := &incus.ConnectionArgs{
		TLSClientCert: string(cert),
		TLSClientKey:  string(key),
		TLSServerCert: serverCert,
		UserAgent:     "incus-console/0.1",
		// Don't listen for events on the event bus — we only need the console.
		SkipGetEvents: true,
	}
	srv, err := incus.ConnectIncusWithContext(ctx, cfg.URL, args)
	if err != nil {
		return nil, fmt.Errorf("connect incus %s: %w", cfg.URL, err)
	}
	// Scope subsequent API calls to the configured project so VMs in
	// non-default projects (e.g. "unidoc") actually show up in the picker.
	if cfg.Project != "" {
		srv = srv.UseProject(cfg.Project)
	}
	return &Server{Client: srv}, nil
}

// VM is a lightweight instance descriptor for the picker UI. Only what the
// UI needs — full incus.InstanceFull is huge.
type VM struct {
	Name    string
	Status  string // "Running", "Stopped", ...
	Type    string // "container", "virtual-machine"
	Project string
	// Architecture is the incus-side arch string, e.g. "x86_64",
	// "aarch64". Used by the agent installer to pick the right
	// incus-agent binary.
	Architecture string
	// OS is the guest OS name incus knows about (from image.os config
	// or the running agent). Empty for freshly-installed VMs where the
	// OS type isn't known yet. Used together with Architecture to key
	// into Config.AgentBinaries.
	OS string
}

// Identity captures who this client cert authenticates as on the
// incus daemon plus a short summary of the granted permissions and
// the server's version + build info. Shown in the sidebar and host
// info panel so the operator sees at a glance which cert is in use,
// what it can do, and which agent binary matches the server's train.
type Identity struct {
	Fingerprint   string   // truncated to 12 chars for display
	Type          string   // "client", "server", "metrics", …
	Restricted    bool     // true if the trust is restricted to specific projects
	Projects      []string // projects the cert can see (nil when unrestricted)
	ServerName    string   // incus server / cluster name
	ServerType    string   // "standalone" or "cluster"
	ServerVersion string   // "6.0.1" — used to pick a matching incus-agent
	Kernel        string   // "Linux 6.6.…"
	Architecture  string   // "x86_64", "aarch64", …
}

// WhoAmI queries the server for identity + high-level environment info
// so the UI can show a "you are cert:abc123 / cluster: unidoc" summary.
// Best-effort — a partial response is better than none, so most errors
// still return a non-nil Identity with whichever fields succeeded.
func (s *Server) WhoAmI() (*Identity, error) {
	srv, _, err := s.Client.GetServer()
	if err != nil {
		return nil, err
	}
	id := &Identity{
		ServerName:    srv.Environment.ServerName,
		ServerType:    "standalone",
		ServerVersion: srv.Environment.ServerVersion,
		Kernel:        srv.Environment.Kernel + " " + srv.Environment.KernelVersion,
		Architecture:  srv.Environment.KernelArchitecture,
	}
	if srv.Environment.ServerClustered {
		id.ServerType = "cluster"
	}
	// srv.AuthUserName is our cert fingerprint when Auth == "trusted".
	// Restricted certs typically can't GET other people's certificate
	// records, but they CAN get their own, so this call works for both
	// admin and project-restricted trusts.
	if srv.Auth == "trusted" && srv.AuthUserName != "" {
		fp := srv.AuthUserName
		id.Fingerprint = fp
		if len(id.Fingerprint) > 12 {
			id.Fingerprint = id.Fingerprint[:12]
		}
		if cert, _, err := s.Client.GetCertificate(fp); err == nil && cert != nil {
			id.Type = cert.Type
			id.Restricted = cert.Restricted
			id.Projects = cert.Projects
		}
	}
	return id, nil
}

// ListProjects returns the project names visible to the client cert.
// Used by the sidebar tree so the user can drill into any project on the
// host, not just the one wired into the Host config.
func (s *Server) ListProjects() ([]string, error) {
	names, err := s.Client.GetProjectNames()
	if err != nil {
		return nil, err
	}
	return names, nil
}

// InstanceDetail is the full instance record incus returns from
// GET /1.0/instances/{name}. We pass it back to the UI verbatim so
// the Info panel can render anything the operator cares about
// (memory limits, network devices, image metadata, etc).
type InstanceDetail = api.InstanceFull

// GetInstanceDetail fetches everything incus knows about one instance
// in the given project. Used by the "Info" panel.
func (s *Server) GetInstanceDetail(project, name string) (*InstanceDetail, error) {
	c := s.Client.UseProject(project)
	inst, _, err := c.GetInstanceFull(name)
	if err != nil {
		return nil, err
	}
	return inst, nil
}

// ListVMsInProject scopes the client to the given project and returns
// ALL instances (VMs + containers). The Type field distinguishes them
// so the sidebar can render appropriately.
func (s *Server) ListVMsInProject(project string) ([]VM, error) {
	c := s.Client.UseProject(project)
	insts, err := c.GetInstances(api.InstanceTypeAny)
	if err != nil {
		return nil, err
	}
	out := make([]VM, 0, len(insts))
	for _, i := range insts {
		out = append(out, VM{
			Name:         i.Name,
			Status:       i.Status,
			Type:         i.Type,
			Project:      project,
			Architecture: i.Architecture,
			OS:           i.Config["image.os"],
		})
	}
	return out, nil
}

// OpenConsoleInProject is the project-aware version of OpenConsole — used
// by the tree UI where the project is per-VM, not per-host.
func (s *Server) OpenConsoleInProject(project, vmName string) (*SpiceConsole, error) {
	c := s.Client.UseProject(project)
	scoped := &Server{Client: c}
	return scoped.OpenConsole(vmName)
}

// Shell holds an open interactive bash session on a container. Read/Write
// are the guest's stdout and stdin respectively — from the caller's view
// it looks like an io.ReadWriteCloser wrapping a remote PTY.
type Shell struct {
	stdinW     io.WriteCloser
	stdoutR    io.ReadCloser
	op         incus.Operation
	disconnect chan bool
	done       chan bool
}

// OpenShellInProject launches `bash` interactively inside the given
// container and returns a duplex handle to its PTY. Callers Read for
// guest output, Write for guest input.
func (s *Server) OpenShellInProject(project, vmName string) (*Shell, error) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	disc := make(chan bool, 1)
	done := make(chan bool, 1)

	args := &incus.InstanceExecArgs{
		Stdin:  stdinR,
		Stdout: stdoutW,
		Stderr: stdoutW,
		// Interactive exec still gets a control WS for window resize
		// etc — we just drain it so its buffer never fills.
		Control: func(ws *websocket.Conn) {
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
			}
		},
		DataDone: done,
	}

	c := s.Client.UseProject(project)
	// Interactive PTY exec doesn't cleanly report "command not found" —
	// if bash isn't there, the PTY just closes immediately with no
	// error at the client library level. So we probe with a fast
	// non-interactive `command -v bash` first and pick the shell that
	// actually exists.
	shellCmd := probeShell(c, vmName)

	// TERM=xterm-256color enables full ANSI colours and cursor
	// addressing — the terminal widget in the UI handles all of it.
	op, err := c.ExecInstance(vmName, api.InstanceExecPost{
		Command:     []string{shellCmd},
		Interactive: true,
		WaitForWS:   true,
		Width:       120,
		Height:      36,
		Environment: map[string]string{
			"TERM": "xterm-256color",
			"HOME": "/root",
			"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			// Override GNU ls default which paints directories in dark
			// blue (unreadable on a black terminal background). Bright
			// cyan is the ubiquitous "solarized dark" / dracula / gruvbox
			// choice; matches what tmux + zsh + iTerm2 users are used to.
			"LS_COLORS": "rs=0:di=1;36:ln=1;35:mh=0:pi=40;33:so=1;35:do=1;35:bd=40;33;01:cd=40;33;01:or=40;31;01:mi=00:su=37;41:sg=30;43:ca=00:tw=30;42:ow=34;42:st=37;44:ex=1;32:*.tar=1;31:*.zip=1;31:*.gz=1;31:*.bz2=1;31:*.xz=1;31:*.7z=1;31:*.deb=1;31:*.rpm=1;31:*.md=1;33:*.txt=0:*.json=0;36:*.yaml=0;36:*.yml=0;36:*.sh=1;32",
			// Give bash a decent prompt with color that survives across
			// PROMPT_COMMAND overrides in the guest.
			"PS1": `\[\e[1;36m\]\u@\h\[\e[0m\]:\[\e[1;33m\]\w\[\e[0m\]\$ `,
		},
	}, args)
	if err != nil {
		_ = stdinW.Close()
		_ = stdoutR.Close()
		return nil, err
	}

	sh := &Shell{
		stdinW:     stdinW,
		stdoutR:    stdoutR,
		op:         op,
		disconnect: disc,
		done:       done,
	}
	go func() {
		<-done
		_ = stdoutW.Close()
	}()
	return sh, nil
}

// probeShell asks the container which of the preferred shells exists,
// via a non-interactive exec that just runs `command -v`. Falls back to
// "sh" on any error — sh is guaranteed to exist on every POSIX guest.
func probeShell(c incus.InstanceServer, vmName string) string {
	for _, sh := range []string{"bash", "sh"} {
		op, err := c.ExecInstance(vmName, api.InstanceExecPost{
			Command:     []string{"sh", "-c", "command -v " + sh + " >/dev/null 2>&1"},
			Interactive: false,
			WaitForWS:   true,
		}, &incus.InstanceExecArgs{})
		if err != nil {
			continue
		}
		if err := op.Wait(); err != nil {
			continue
		}
		if md := op.Get().Metadata; md != nil {
			if rc, ok := md["return"].(float64); ok && rc == 0 {
				return sh
			}
		}
	}
	return "sh"
}

func (s *Shell) Read(p []byte) (int, error)  { return s.stdoutR.Read(p) }
func (s *Shell) Write(p []byte) (int, error) { return s.stdinW.Write(p) }
func (s *Shell) Close() error {
	_ = s.stdinW.Close()
	_ = s.stdoutR.Close()
	select {
	case s.disconnect <- true:
	default:
	}
	return nil
}

// ResetVMInProject — project-scoped ResetVM.
func (s *Server) ResetVMInProject(project, vmName string) error {
	c := s.Client.UseProject(project)
	scoped := &Server{Client: c}
	return scoped.ResetVM(vmName)
}

// ResetVM issues a hard restart on the VM — same as the "Reset" button on
// a physical machine. `force=true` skips the graceful shutdown attempt,
// which is what you want when the guest is stuck (netboot loop, BSOD).
func (s *Server) ResetVM(name string) error {
	op, err := s.Client.UpdateInstanceState(name, api.InstanceStatePut{
		Action:  "restart",
		Force:   true,
		Timeout: -1,
	}, "")
	if err != nil {
		return fmt.Errorf("restart %s: %w", name, err)
	}
	if err := op.Wait(); err != nil {
		return fmt.Errorf("restart %s wait: %w", name, err)
	}
	return nil
}

// setInstanceState is the shared plumbing for start/stop/restart/freeze —
// they all hit the same InstanceStatePut endpoint with different action
// strings. Force is only honoured for stop and restart; the incus server
// ignores it for start/freeze/unfreeze.
func (s *Server) setInstanceState(project, name, action string, force bool) error {
	c := s.Client.UseProject(project)
	op, err := c.UpdateInstanceState(name, api.InstanceStatePut{
		Action:  action,
		Force:   force,
		Timeout: -1,
	}, "")
	if err != nil {
		return fmt.Errorf("%s %s: %w", action, name, err)
	}
	if err := op.Wait(); err != nil {
		return fmt.Errorf("%s %s wait: %w", action, name, err)
	}
	return nil
}

// StartInstance boots the instance. No force flag — incus treats start as
// idempotent (starting an already-running instance is an error, not a
// crash), so callers should check status first.
func (s *Server) StartInstance(project, name string) error {
	return s.setInstanceState(project, name, "start", false)
}

// StopInstance halts the instance. `force=true` skips the ACPI shutdown
// grace period — use when the guest is unresponsive.
func (s *Server) StopInstance(project, name string, force bool) error {
	return s.setInstanceState(project, name, "stop", force)
}

// RestartInstance reboots — graceful if force=false, hard reset if true.
func (s *Server) RestartInstance(project, name string, force bool) error {
	return s.setInstanceState(project, name, "restart", force)
}

// FreezeInstance pauses the guest (SIGSTOP for containers, QEMU pause for
// VMs). Useful to hold state without shutting down.
func (s *Server) FreezeInstance(project, name string) error {
	return s.setInstanceState(project, name, "freeze", false)
}

// UnfreezeInstance resumes a frozen guest.
func (s *Server) UnfreezeInstance(project, name string) error {
	return s.setInstanceState(project, name, "unfreeze", false)
}

// ClusterMember is our thin, UI-friendly projection of the incus API's
// full ClusterMember — just the fields the info panel renders.
type ClusterMember struct {
	Name         string
	URL          string
	Status       string   // Online / Offline / Blocked / …
	Message      string   // "fully operational", etc.
	Roles        []string // database, event-hub, …
	Architecture string
	Database     bool
}

// ListClusterMembers returns the cluster's members. On a standalone
// (non-clustered) server this returns an empty slice + nil error — the
// caller decides whether to render the section.
func (s *Server) ListClusterMembers() ([]ClusterMember, error) {
	members, err := s.Client.GetClusterMembers()
	if err != nil {
		// Non-clustered servers return an error here; treat as empty
		// so the info panel doesn't turn a happy state into a red row.
		return nil, nil
	}
	out := make([]ClusterMember, 0, len(members))
	for _, m := range members {
		out = append(out, ClusterMember{
			Name:         m.ServerName,
			URL:          m.URL,
			Status:       m.Status,
			Message:      m.Message,
			Roles:        m.Roles,
			Architecture: m.Architecture,
			Database:     m.Database,
		})
	}
	return out, nil
}

// Network is our UI-friendly projection of api.Network — enough to
// render a read-only table without dragging the whole incus schema
// into the UI layer.
type Network struct {
	Name    string
	Type    string // bridge, ovn, physical, macvlan, …
	Managed bool   // is incus managing this network?
	Status  string // Created / Errored / Pending
	UsedBy  int    // count of consumers (instances, profiles, …)
}

// ListNetworksInProject returns the networks visible in one project.
// Read-only — this tool never creates or edits networks.
func (s *Server) ListNetworksInProject(project string) ([]Network, error) {
	c := s.Client.UseProject(project)
	nets, err := c.GetNetworks()
	if err != nil {
		return nil, err
	}
	out := make([]Network, 0, len(nets))
	for _, n := range nets {
		out = append(out, Network{
			Name:    n.Name,
			Type:    n.Type,
			Managed: n.Managed,
			Status:  n.Status,
			UsedBy:  len(n.UsedBy),
		})
	}
	return out, nil
}

// NetworkACL is the UI-friendly projection of api.NetworkACL. Rules are
// summarised as counts — the actual match/action strings live in incus
// CLI territory.
type NetworkACL struct {
	Name        string
	Description string
	Ingress     int // number of ingress rules
	Egress      int // number of egress rules
	UsedBy      int
}

// ListNetworkACLsInProject returns the ACLs visible in one project.
// Read-only — see [feedback_incus_console_scope]: this tool is for
// interpreting the network posture, not mutating it.
func (s *Server) ListNetworkACLsInProject(project string) ([]NetworkACL, error) {
	c := s.Client.UseProject(project)
	acls, err := c.GetNetworkACLs()
	if err != nil {
		return nil, err
	}
	out := make([]NetworkACL, 0, len(acls))
	for _, a := range acls {
		out = append(out, NetworkACL{
			Name:        a.Name,
			Description: a.Description,
			Ingress:     len(a.Ingress),
			Egress:      len(a.Egress),
			UsedBy:      len(a.UsedBy),
		})
	}
	return out, nil
}

// ListVMs returns instances of type "virtual-machine" — SPICE only makes
// sense for VMs (containers don't have a framebuffer). Now also captures
// Architecture + OS so the agent installer can pick the right binary.
func (s *Server) ListVMs() ([]VM, error) {
	insts, err := s.Client.GetInstances(api.InstanceTypeVM)
	if err != nil {
		return nil, err
	}
	out := make([]VM, 0, len(insts))
	for _, i := range insts {
		out = append(out, VM{
			Name:         i.Name,
			Status:       i.Status,
			Type:         i.Type,
			Project:      i.Project,
			Architecture: i.Architecture,
			OS:           i.Config["image.os"],
		})
	}
	return out, nil
}

// SpiceConsole holds an open incus SPICE console. Each call to SpiceConnect
// opens a new tunneled connection to the underlying QEMU SPICE socket.
//
// Implements the github.com/unidoc/spice.Connector interface.
type SpiceConsole struct {
	op         incus.Operation
	openConn   func(io.ReadWriteCloser) error
	disconnect chan bool

	// OnMirrorEnded, if set, is fired the first time an incus mirror
	// goroutine (i.e. a live SPICE channel) exits. Signals that the VM
	// went away — used by the UI to switch the console to a "Disconnected"
	// state without having to poll incus.
	OnMirrorEnded func()
	onceEnded     sync.Once
}

// OpenConsole starts a SPICE session for the given VM. Returns a connector
// the SPICE client library can use to open all its channels.
//
// The console stays alive until Close() — or until the incus operation is
// cancelled server-side (VM shutdown, admin kick). While alive, incus is
// running the SPICE server socket and accepting new tunnelled connections
// via `openConn`.
func (s *Server) OpenConsole(vmName string) (*SpiceConsole, error) {
	disc := make(chan bool, 1)
	args := &incus.InstanceConsoleArgs{
		// Control channel — incus opens a separate WebSocket for control
		// frames (resize hints, credentials refresh, keepalive). We don't
		// need to act on any of them for a basic client, but we must
		// drain the WS or its buffer fills and the server stalls.
		Control: func(ws *websocket.Conn) {
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
			}
		},
		ConsoleDisconnect: disc,
	}
	op, opener, err := s.Client.ConsoleInstanceDynamic(
		vmName,
		api.InstanceConsolePost{Type: "vga"},
		args,
	)
	if err != nil {
		return nil, fmt.Errorf("open vga console for %s: %w", vmName, err)
	}
	return &SpiceConsole{op: op, openConn: opener, disconnect: disc}, nil
}

// SpiceConnect implements spice.Connector. The `compress` flag is a hint
// from the SPICE library that this channel would benefit from QUIC/LZ
// image compression — irrelevant to our transport (incus tunnels raw
// SPICE bytes) so we ignore it.
//
// Uses a socketpair rather than net.Pipe() on purpose: net.Pipe is
// unbuffered, so a large Write from ws.Mirror blocks until the SPICE
// library has read every last byte via its small (4-byte header, N-byte
// body) reads. That deadlocks the CHANNELS_LIST handshake even though
// QEMU logs SPICE_CONNECTED. Kernel-backed socketpair gives ~64KB of
// buffering per direction — plenty for SPICE's protocol chunks.
//
// The incus factory `openConn(rwc)` is BLOCKING — it runs `ws.Mirror`
// inline and only returns when the SPICE channel is closed. So we spawn
// it in a goroutine and return to the SPICE library immediately with the
// local end of the pipe.
func (c *SpiceConsole) SpiceConnect(_ bool) (net.Conn, error) {
	log.Printf("incus-console: opening new SPICE channel via incus websocket")
	local, remote, err := socketPair()
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	go func() {
		if err := c.openConn(remote); err != nil {
			log.Printf("incus-console: SPICE channel mirror ended with error: %v", err)
		}
		// openConn owns the remote end (ws.Mirror closes it when done)
		// but calling Close twice on a net.Conn is a no-op — safe.
		_ = remote.Close()
		// First channel to die takes the VM offline. Fire once — the
		// remaining channels will exit on their own after their own
		// mirrors return.
		c.onceEnded.Do(func() {
			if c.OnMirrorEnded != nil {
				c.OnMirrorEnded()
			}
		})
	}()
	log.Printf("incus-console: SPICE channel handed to spice lib")
	return local, nil
}

// socketPair returns two connected AF_UNIX stream endpoints wrapped as
// net.Conn — each direction has kernel socket buffering (default ~64KB
// on Linux/macOS), unlike net.Pipe() which is fully synchronous.
func socketPair() (net.Conn, net.Conn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	f0 := os.NewFile(uintptr(fds[0]), "spice-local")
	f1 := os.NewFile(uintptr(fds[1]), "spice-remote")
	// FileConn dup's the fd internally and takes ownership; close our
	// original *os.File wrappers now that FileConn has its own copies.
	local, err := net.FileConn(f0)
	_ = f0.Close()
	if err != nil {
		_ = f1.Close()
		return nil, nil, err
	}
	remote, err := net.FileConn(f1)
	_ = f1.Close()
	if err != nil {
		_ = local.Close()
		return nil, nil, err
	}
	return local, remote, nil
}

// Close terminates the SPICE console and all its tunnelled channels.
func (c *SpiceConsole) Close() error {
	select {
	case c.disconnect <- true:
	default:
	}
	// Give the operation a moment to unwind before we return, so callers
	// that immediately reconnect don't race the server-side cleanup.
	time.Sleep(50 * time.Millisecond)
	return nil
}
