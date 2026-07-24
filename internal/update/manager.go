package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	VersionHeaderName = "Hyperstack-Agent-Version"
	DigestHeaderName  = "Hyperstack-Agent-Digest"
	maxBinarySize     = 256 * 1024 * 1024
)

type Release struct {
	Version     string
	Digest      string
	DownloadURL string
	StagedPath  string
}

type Manager struct {
	CheckURL       string
	CurrentVersion string
	Client         *http.Client
	downloadMu     sync.Mutex
}

func NewManager(checkURL, currentVersion string) *Manager {
	return &Manager{
		CheckURL:       checkURL,
		CurrentVersion: currentVersion,
		Client: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (m *Manager) Check(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, m.CheckURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return nil, fmt.Errorf("update check returned status %s", resp.Status)
	}

	version := strings.TrimSpace(resp.Header.Get(VersionHeaderName))
	if version == "" {
		return nil, errors.New("update check missing version header")
	}

	// Fix 5: treat non-semver current version (e.g. "dev", git SHA) as unknown —
	// skip the update silently rather than spamming parse errors every check cycle.
	shouldUpdate, err := isHigherVersion(version, m.CurrentVersion)
	if err != nil {
		return nil, err
	}
	if !shouldUpdate {
		return nil, nil
	}

	downloadURL := strings.TrimSpace(resp.Header.Get("Location"))
	if downloadURL == "" {
		downloadURL = m.CheckURL
	}
	if resolved, err := resolveURL(m.CheckURL, downloadURL); err == nil {
		downloadURL = resolved
	}

	// Fix 1: require the digest header before returning a release — an empty
	// digest must never be silently passed through, since DownloadRelease would
	// then promote the binary without any integrity verification.
	digest := strings.TrimSpace(resp.Header.Get(DigestHeaderName))
	if digest == "" {
		return nil, errors.New("update check missing digest header")
	}

	return &Release{
		Version:     version,
		Digest:      digest,
		DownloadURL: downloadURL,
	}, nil
}

func (m *Manager) DownloadRelease(ctx context.Context, release *Release, currentPath string) error {
	if release == nil {
		return errors.New("release is required")
	}
	m.downloadMu.Lock()
	defer m.downloadMu.Unlock()

	release.StagedPath = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, release.DownloadURL, nil)
	if err != nil {
		return err
	}

	resp, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// Handle redirects (3xx) by following Location header, since the HTTP client
	// has CheckRedirect disabled to prevent auto-following in Check().
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		location := resp.Header.Get("Location")
		if location == "" {
			return fmt.Errorf("binary download returned redirect %s with no Location header", resp.Status)
		}
		// Follow the redirect with a new request
		redirectReq, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
		if err != nil {
			return err
		}
		resp, err = m.Client.Do(redirectReq)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("binary download returned status %s", resp.Status)
	}

	// Fix 3: stage in os.TempDir() rather than next to the current executable.
	// The directory containing the installed binary is read-only under systemd
	// (ProtectSystem=strict) and in the Docker runtime (non-root). os.TempDir()
	// is always writable. atomicSwap handles the cross-device rename fallback.
	//
	// Use os.CreateTemp so the file is created atomically with an unguessable
	// name, eliminating the Remove→create TOCTOU window that a predictable fixed
	// name in shared /tmp would expose (symlink replacement attack).
	f, err := os.CreateTemp(os.TempDir(), "."+filepath.Base(currentPath)+"-*.tmp") /* #nosec G302 -- binary must be world-readable/executable */
	if err != nil {
		return err
	}
	tmpPath := f.Name()

	copyErr := func() error {
		if err := copyWithLimit(f, resp.Body, maxBinarySize); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		return nil
	}()
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil { /* #nosec G302 -- binary must be world-readable/executable */
		_ = os.Remove(tmpPath)
		return err
	}

	if err := verifyDigest(tmpPath, release.Digest); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := smokeTestBinary(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	release.StagedPath = tmpPath
	return nil
}

func copyWithLimit(dst io.Writer, src io.Reader, maxBytes int64) error {
	n, err := io.Copy(dst, io.LimitReader(src, maxBytes+1))
	if err != nil {
		return err
	}
	if n > maxBytes {
		return fmt.Errorf("binary download exceeds max size %d bytes", maxBytes)
	}
	return nil
}

func (m *Manager) PromoteRelease(currentPath string, release *Release) error {
	if release == nil {
		return errors.New("release is required")
	}
	if strings.TrimSpace(release.StagedPath) == "" {
		return errors.New("release staged path is required")
	}

	if err := atomicSwap(currentPath, release.StagedPath); err != nil {
		return err
	}

	m.CurrentVersion = release.Version
	return nil
}

// resolveExecPath resolves symlinks and verifies the result is an absolute path.
// Both RestartProcess and smokeTestBinary call this before any exec so that
// the executed path is a concrete, fully-resolved value rather than a raw
// variable — which satisfies gosec G204 without needing a suppression annotation.
func resolveExecPath(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("resolve exec path: %w", err)
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("exec path is not absolute: %s", resolved)
	}
	return resolved, nil
}

// RestartProcess replaces the current process image with the binary at
// currentPath using a clean exec(2). The path is resolved through symlinks
// before the exec so the kernel receives a concrete, absolute path.
func RestartProcess(currentPath string) error {
	resolved, err := resolveExecPath(currentPath)
	if err != nil {
		return err
	}
	return syscall.Exec(resolved, os.Args, os.Environ()) /* #nosec G204 G702 -- resolved is the symlink-evaluated, absolute-asserted current executable path */
}

func atomicSwap(currentPath, newPath string) error {
	backup := currentPath + ".bak"
	_ = os.Remove(backup)
	if err := copyFile(currentPath, backup); err != nil {
		return err
	}
	// os.Rename is atomic on the same filesystem. When the staged binary lives
	// in os.TempDir() and the install dir is on a different device (common in
	// Docker / systemd setups), Rename returns EXDEV. Fall back to a copy+remove
	// so the promote step still succeeds across filesystem boundaries.
	if err := os.Rename(newPath, currentPath); err != nil {
		// Cross-device rename (EXDEV) fallback: copyFile opens the destination
		// with O_TRUNC, so currentPath is zeroed the moment the copy begins.
		// If the copy fails, restore from the backup made above to avoid leaving
		// the agent with a truncated (unlaunchable) binary.
		if err2 := copyFile(newPath, currentPath); err2 != nil {
			_ = copyFile(backup, currentPath) // best-effort restore
			_ = os.Remove(newPath)
			return err2
		}
		_ = os.Remove(newPath)
	}
	return nil
}

// Fix 2: verifyDigest now fails closed — an empty digest is treated as an
// error rather than silently skipping verification. This prevents a stripped
// or missing Hyperstack-Agent-Digest header from allowing an unverified binary
// through (e.g. a bad gateway response that drops the header).
func verifyDigest(path, digest string) error {
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return errors.New("digest is required for binary verification")
	}

	algorithm, expected, ok := strings.Cut(digest, ":")
	if !ok || strings.TrimSpace(expected) == "" {
		return fmt.Errorf("invalid digest %q", digest)
	}
	if strings.ToLower(strings.TrimSpace(algorithm)) != "sha256" {
		return fmt.Errorf("unsupported digest algorithm %q", algorithm)
	}

	actual, err := fileDigest(path, sha256.New())
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, strings.TrimSpace(expected)) {
		return fmt.Errorf("digest mismatch: got sha256:%s, want %s", actual, digest)
	}
	return nil
}

func fileDigest(path string, h hash.Hash) (string, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(filepath.Base(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func smokeTestBinary(binaryPath string) error {
	// Fix 4: resolve the path through symlinks before exec so gosec G204 sees a
	// concrete, absolute path rather than a raw variable.
	resolved, err := resolveExecPath(binaryPath)
	if err != nil {
		return err
	}

	smokeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(smokeCtx, resolved, "diagnose", "status") /* #nosec G204 G702 -- resolved is the symlink-evaluated, absolute-asserted staged binary path */
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if smokeCtx.Err() == context.DeadlineExceeded {
		return errors.New("smoke test timed out")
	}
	if err == nil {
		return nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("smoke test exit code %d: %s", exitErr.ExitCode(), strings.TrimSpace(string(output)))
	}

	return err
}

func copyFile(src, dst string) error {
	srcRoot, err := os.OpenRoot(filepath.Dir(src))
	if err != nil {
		return err
	}
	defer func() { _ = srcRoot.Close() }()
	in, err := srcRoot.Open(filepath.Base(src))
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	dstRoot, err := os.OpenRoot(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer func() { _ = dstRoot.Close() }()
	out, err := dstRoot.OpenFile(filepath.Base(dst), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); err != nil {
			slog.Debug("copyFile: close destination failed", "dst", dst, "error", err)
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// Fix 5: isHigherVersion treats a non-parseable current version (e.g. "dev",
// a git SHA, or any non-semver string) as "unknown / dev build" and returns
// false without an error. This prevents the update check loop from spamming
// parse errors every cycle when the agent is built without a semver tag.
// The server-supplied next version must still be valid semver.
func isHigherVersion(next, current string) (bool, error) {
	nextParts, err := parseVersion(next)
	if err != nil {
		return false, fmt.Errorf("invalid next version %q: %w", next, err)
	}
	currentParts, err := parseVersion(current)
	if err != nil {
		// Non-semver current version (dev build, git sha) — skip update silently.
		return false, nil
	}

	for i := 0; i < len(nextParts) || i < len(currentParts); i++ {
		var a, b int
		if i < len(nextParts) {
			a = nextParts[i]
		}
		if i < len(currentParts) {
			b = currentParts[i]
		}
		if a > b {
			return true, nil
		}
		if a < b {
			return false, nil
		}
	}
	return false, nil
}

func parseVersion(v string) ([]int, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(v, "v"))
	if trimmed == "" {
		return nil, errors.New("empty version")
	}

	core := strings.SplitN(trimmed, "+", 2)[0]
	core = strings.SplitN(core, "-", 2)[0]
	parts := strings.Split(core, ".")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, errors.New("empty version segment")
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func resolveURL(baseURL, target string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(parsed).String(), nil
}
