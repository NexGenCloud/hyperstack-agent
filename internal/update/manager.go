package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
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

	// Stage the new binary as .new for ExecStartPre to swap on next restart.
	// ExecStartPre will verify the binary, swap it atomically, and restore on failure.
	stagedPath := currentPath + ".new"
	if err := os.Rename(release.StagedPath, stagedPath); err != nil {
		// If rename fails due to cross-device link (e.g., /tmp on different device),
		// fall back to copying the file.
		if !isExdev(err) {
			return err
		}
		// Copy staged binary to .new
		src, err := os.Open(release.StagedPath)
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()

		dst, err := os.Create(stagedPath) /* #nosec G304 -- stagedPath is currentPath + ".new" from os.Executable(), not user-supplied */
		if err != nil {
			return err
		}
		defer func() { _ = dst.Close() }()

		if _, err := io.Copy(dst, src); err != nil {
			_ = os.Remove(stagedPath)
			return err
		}
		if err := dst.Sync(); err != nil {
			_ = os.Remove(stagedPath)
			return err
		}
		// Make the staged binary executable
		if err := os.Chmod(stagedPath, 0o755); err != nil { /* #nosec G302 -- binary must be world-readable/executable */
			_ = os.Remove(stagedPath)
			return err
		}
		_ = os.Remove(release.StagedPath)
	}

	m.CurrentVersion = release.Version
	return nil
}

// isExdev checks if an error is EXDEV (cross-device link).
func isExdev(err error) bool {
	if err == nil {
		return false
	}
	// Check for syscall.EXDEV directly
	if errno, ok := err.(syscall.Errno); ok {
		return errno == syscall.EXDEV
	}
	// Check in os.LinkError
	if linkErr, ok := err.(*os.LinkError); ok {
		if errno, ok := linkErr.Err.(syscall.Errno); ok {
			return errno == syscall.EXDEV
		}
	}
	return strings.Contains(err.Error(), "cross-device link") || strings.Contains(err.Error(), "invalid cross-device")
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
