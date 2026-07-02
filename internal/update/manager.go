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

	return &Release{
		Version:     version,
		Digest:      strings.TrimSpace(resp.Header.Get(DigestHeaderName)),
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

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("binary download returned status %s", resp.Status)
	}

	dir := filepath.Dir(currentPath)
	tmpPath := filepath.Join(dir, "."+filepath.Base(currentPath)+".tmp")
	if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}

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

	if err := os.Chmod(tmpPath, 0o755); err != nil {
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

func RestartProcess(currentPath string) error {
	return syscall.Exec(currentPath, os.Args, os.Environ())
}

func atomicSwap(currentPath, newPath string) error {
	backup := currentPath + ".bak"
	_ = os.Remove(backup)
	if err := copyFile(currentPath, backup); err != nil {
		return err
	}
	return os.Rename(newPath, currentPath)
}

func verifyDigest(path, digest string) error {
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return nil
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
	f, err := os.Open(path)
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
	smokeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(smokeCtx, binaryPath, "diagnose", "status")
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
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
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

func isHigherVersion(next, current string) (bool, error) {
	nextParts, err := parseVersion(next)
	if err != nil {
		return false, fmt.Errorf("invalid next version %q: %w", next, err)
	}
	currentParts, err := parseVersion(current)
	if err != nil {
		return false, fmt.Errorf("invalid current version %q: %w", current, err)
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
