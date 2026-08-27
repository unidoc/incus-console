package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReleaseURLTemplate is the download-URL pattern for incus-agent
// binaries built and published alongside incus-console. `{version}` is
// the incus server version (bare, no leading v — matches the incus
// upstream release train), and `{filename}` is the platform-suffixed
// binary name (`incus-agent-windows-amd64.exe`,
// `incus-agent-linux-arm64`, …).
//
// Overridable at runtime via the AGENT_RELEASE_URL_TEMPLATE env var so
// air-gapped installs can point at an internal mirror.
const ReleaseURLTemplate = "https://github.com/unidoc/incus-console/releases/download/agent-v{version}/{filename}"

// Filename returns the canonical incus-agent filename for a Go
// os/arch pair. Windows targets get an .exe suffix; everything else
// stays bare so the file is directly executable on Unix hosts.
func Filename(goos, goarch string) string {
	name := "incus-agent-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// resolveTemplate substitutes {version} and {filename} in a URL
// template. Kept as a plain function so the env-override path is
// covered by the same substitution logic.
func resolveTemplate(tmpl, version, filename string) (string, error) {
	out := strings.ReplaceAll(tmpl, "{version}", version)
	out = strings.ReplaceAll(out, "{filename}", filename)
	if _, err := url.Parse(out); err != nil {
		return "", fmt.Errorf("invalid template: %w", err)
	}
	return out, nil
}

// Fetch downloads the incus-agent binary matching (version, goos,
// goarch) into destPath, atomically. The download is streamed to a
// temp file next to destPath and renamed on success, so a killed run
// never leaves a half-written binary behind. Executable bit is set on
// non-Windows targets.
//
// This function is intentionally offline-friendly: any failure returns
// a wrapped error the UI layer can surface — nothing panics, nothing
// retries. Retry is the caller's decision.
func Fetch(ctx context.Context, version, goos, goarch, destPath string) error {
	if version == "" {
		return fmt.Errorf("no server version — connect to a host first so we know which agent train to pull")
	}
	tmpl := os.Getenv("AGENT_RELEASE_URL_TEMPLATE")
	if tmpl == "" {
		tmpl = ReleaseURLTemplate
	}
	filename := Filename(goos, goarch)
	dl, err := resolveTemplate(tmpl, version, filename)
	if err != nil {
		return err
	}

	// Ensure the target directory exists so users can point at
	// ~/.config/incus-console/agents/... without pre-creating it.
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(destPath), err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dl, nil)
	if err != nil {
		return fmt.Errorf("request %s: %w", dl, err)
	}
	req.Header.Set("User-Agent", "incus-console/agent-fetcher")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", dl, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Read a small excerpt of the body for the error message —
		// GitHub returns a helpful "not found" JSON blob we don't
		// want to log wholesale.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("download %s: HTTP %d — %s", dl, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	tmp, err := os.CreateTemp(filepath.Dir(destPath), filepath.Base(destPath)+".partial-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup on error — nothing to log if it fails.
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}

	// Executable owner-only. 0o755 world-readable was overkill: the
	// binary is going to be shipped to a VM's SYSTEM context and used
	// to authenticate as the guest agent — an unprivileged local
	// process getting a copy of the binary itself isn't a leak, but
	// the containing parent dir is 0o700 so 0o700 on the file too
	// keeps the whole tree owner-only. Windows ignores mode bits.
	if goos != "windows" {
		if err := os.Chmod(tmpPath, 0o700); err != nil {
			return fmt.Errorf("chmod: %w", err)
		}
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	return nil
}
