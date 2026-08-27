// Package config persists the user's known incus hosts and the paths to
// their TLS client cert/key. The store lives at ~/.config/incus-console/config.yaml
// so nothing GUI-side needs to know about paths.
package config

import (
	"errors"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Host is one incus daemon the user can connect to.
type Host struct {
	// Name shown in the UI (e.g. "alpine-01").
	Name string `yaml:"name"`
	// URL of the incus API, e.g. "https://alpine-01.internal:8443".
	URL string `yaml:"url"`
	// Project scopes the VM listing (default: "default"). Set to whatever
	// project holds the VMs you want to see — `incus project list` on the
	// host shows the options.
	Project string `yaml:"project,omitempty"`
	// TLSClientCert is the PEM file with the client certificate provisioned
	// on this host (`incus config trust add`). Overridden by
	// TLSClientCertCommand when both are set.
	TLSClientCert string `yaml:"tls_client_cert,omitempty"`
	// TLSClientKey is the PEM file with the private key for the above cert.
	// Overridden by TLSClientKeyCommand when both are set.
	TLSClientKey string `yaml:"tls_client_key,omitempty"`

	// TLSClientCertCommand is an optional shell command whose stdout is
	// the client certificate PEM. Use this to avoid keeping the cert on
	// disk in plaintext — pipe it through sops/age/1Password/Vault/OS
	// keychain (`op read`, `sops -d`, `vault kv get`, `security find-…`).
	// If set, the command is re-invoked on every new connection so
	// rotated credentials pick up without a restart. The stdout bytes
	// are held in memory only and never written to disk.
	TLSClientCertCommand string `yaml:"tls_client_cert_command,omitempty"`
	// TLSClientKeyCommand is the private-key equivalent of
	// TLSClientCertCommand. Same rules: stdout = PEM, in-memory only,
	// re-invoked per connection.
	TLSClientKeyCommand string `yaml:"tls_client_key_command,omitempty"`

	// TLSClientCertKeychain, when set, tells us to read the client cert
	// from the OS-native keychain under this account name (service is
	// "io.unidoc.incus-console"). Wins over both path and command. Set
	// automatically when the user pastes/drops a cert into the Host
	// form's "System keychain" mode.
	TLSClientCertKeychain string `yaml:"tls_client_cert_keychain,omitempty"`
	// TLSClientKeyKeychain is the private-key equivalent — same
	// service, different account name.
	TLSClientKeyKeychain string `yaml:"tls_client_key_keychain,omitempty"`
	// TLSServerCert is the remote's self-signed cert, pinned. Empty means
	// verify against the system CA (only useful when the daemon runs behind
	// a real cert). Grab with `openssl s_client -connect host:8443` or
	// `curl -k https://host:8443/1.0`.
	TLSServerCert string `yaml:"tls_server_cert,omitempty"`
}

// Config is the top-level persisted state.
type Config struct {
	Hosts []Host `yaml:"hosts"`

	// AgentBinaries maps <os>/<arch> to the local filesystem path to the
	// matching incus-agent binary. Keys are the same strings incus uses
	// in its instance API (e.g. "windows/amd64", "linux/amd64",
	// "linux/arm64"). The "Install incus-agent" flow picks the right
	// entry based on the target VM's architecture. Empty map is fine —
	// installer prompts to configure when needed.
	AgentBinaries map[string]string `yaml:"agent_binaries,omitempty"`
}

// Host looks up a host by name. Returns nil if not found. Pointer is
// stable — points into the Hosts slice, so callers should not retain it
// across Save() calls that may reallocate.
func (c *Config) Host(name string) *Host {
	for i := range c.Hosts {
		if c.Hosts[i].Name == name {
			return &c.Hosts[i]
		}
	}
	return nil
}

// Path returns the canonical location of the config file.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "incus-console", "config.yaml"), nil
}

// Load reads the config file. A missing file is not an error — returns an
// empty Config so first-run behaviour is "no hosts yet, add one".
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	buf, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, err
	}
	c := &Config{}
	if err := yaml.Unmarshal(buf, c); err != nil {
		return nil, err
	}
	return c, nil
}

// Save writes the config file, creating the parent directory if needed.
// Uses a temp-file + rename to keep the on-disk file valid at all times:
// a crash mid-write leaves the previous config intact instead of a
// truncated YAML that reads as "no hosts" and prompts the user to
// re-enter credentials (potentially through less-secure sources).
func (c *Config) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	buf, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	// Same-dir tmpfile so rename(2) is atomic on POSIX + Windows.
	tmp, err := os.CreateTemp(dir, ".config.yaml.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Clean up the temp file if any subsequent step fails.
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(buf); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, p)
}
