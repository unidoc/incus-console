// Package creds is the credential source abstraction for incus-console.
// A credential is any secret we need to hand to the incus API — right
// now the client certificate + private key that authenticate a TLS
// mutual handshake.
//
// Three source shapes are supported, in priority order:
//
//  1. Keychain — PEM stored in the OS-native secret store (macOS
//     Login Keychain, Linux libsecret, Windows Credential Manager). Set
//     up once by pasting or dropping the PEM into the app; the plaintext
//     never touches the filesystem afterwards.
//
//  2. Command — a shell command whose stdout is the PEM. The escape
//     hatch for advanced pipelines (sops+age, 1Password `op`, YubiKey
//     PKCS#11, sshcrypt/ed25519+AES-256-GCM, …). Re-invoked on every
//     new connection so rotated credentials are picked up live.
//
//  3. File — a plaintext PEM file on disk. Simplest, least secure;
//     kept for compatibility and for users who genuinely need a file
//     path (e.g. Vault-provisioned certs written to tmpfs).
//
// The Load function picks whichever is set, in the order above.
package creds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/zalando/go-keyring"
)

// KeychainService is the fixed service name we register secrets under.
// Same on every OS; the account name distinguishes cert vs key vs host.
const KeychainService = "io.unidoc.incus-console"

// Source describes one credential's provenance. Exactly one of the
// three lookup fields should be non-empty; Load picks by priority.
type Source struct {
	// KeychainAccount, if set, is the account name to look up under
	// KeychainService in the OS-native secret store. Empty means the
	// credential is not stored in the keychain.
	KeychainAccount string

	// Command, if set, is a shell command line to run; its stdout is
	// used as the PEM bytes. Command's stderr is included in error
	// messages; stdout is never logged.
	Command string

	// FilePath, if set, is a plaintext PEM file to read.
	FilePath string
}

// Load returns the PEM bytes for this source. label is included in
// error messages so callers can distinguish "client cert" from "client
// key" without wrapping errors themselves.
func Load(ctx context.Context, label string, s Source) ([]byte, error) {
	if s.KeychainAccount != "" {
		v, err := keyring.Get(KeychainService, s.KeychainAccount)
		if err != nil {
			return nil, fmt.Errorf("%s keychain lookup (%s): %w",
				label, s.KeychainAccount, err)
		}
		return []byte(v), nil
	}
	if s.Command != "" {
		cmd := exec.CommandContext(ctx, "sh", "-c", s.Command)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			// Surface only the first line and cap length. Tools like
			// `sops`, `op`, `vault`, `security` occasionally emit
			// partial secret material on decrypt failure (envelope
			// headers, key fingerprints, cached auth tokens) — the
			// audit found this text was flowing straight into the
			// error dialog. Keep just enough to explain the failure.
			return nil, fmt.Errorf("%s command failed: %v (%s)",
				label, err, firstLineTrunc(stderr.String(), 80))
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%s command produced no output", label)
		}
		return out, nil
	}
	if s.FilePath != "" {
		buf, err := os.ReadFile(s.FilePath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", label, err)
		}
		return buf, nil
	}
	return nil, fmt.Errorf("%s: no source configured (keychain, command, or file)", label)
}

// StoreInKeychain writes PEM bytes into the OS keychain under the given
// account name. Overwrites any existing entry. Returns an error the UI
// can surface; the caller should NOT log the pem bytes.
func StoreInKeychain(account string, pem []byte) error {
	if account == "" {
		return errors.New("keychain account name required")
	}
	if len(pem) == 0 {
		return errors.New("nothing to store")
	}
	// go-keyring stores strings; PEM is 7-bit ASCII so no encoding
	// concerns. Overwriting is idempotent — Set replaces.
	return keyring.Set(KeychainService, account, string(pem))
}

// DeleteFromKeychain removes a keychain entry. Missing entry is not an
// error — makes cleanup idempotent when reconfiguring a host.
func DeleteFromKeychain(account string) error {
	if account == "" {
		return nil
	}
	err := keyring.Delete(KeychainService, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// firstLineTrunc returns at most `max` bytes of `s`'s first line,
// with an ellipsis appended when the line was truncated. Used to keep
// third-party stderr from spilling into user-facing error dialogs.
func firstLineTrunc(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
