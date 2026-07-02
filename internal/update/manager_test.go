package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagerCheckNoUpdate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(VersionHeaderName, "1.2.3")
		w.Header().Set(DigestHeaderName, digestFor([]byte("unused")))
		w.Header().Set("Location", "/binary")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	manager := NewManager(server.URL+"/download", "1.2.3")
	release, err := manager.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if release != nil {
		t.Fatalf("Check() release = %+v, want nil", release)
	}
}

func TestManagerCheckIgnoresLowerVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(VersionHeaderName, "1.2.2")
		w.Header().Set(DigestHeaderName, digestFor([]byte("unused")))
		w.Header().Set("Location", "/binary")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	manager := NewManager(server.URL+"/download", "1.2.3")
	release, err := manager.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if release != nil {
		t.Fatalf("Check() release = %+v, want nil", release)
	}
}

func TestManagerCheckFindsUpdate(t *testing.T) {
	digest := digestFor([]byte("binary"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(VersionHeaderName, "2.0.0")
		w.Header().Set(DigestHeaderName, digest)
		w.Header().Set("Location", "/binary")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	manager := NewManager(server.URL+"/download", "1.2.3")
	release, err := manager.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if release == nil {
		t.Fatal("Check() release = nil, want update")
	}
	if release.Version != "2.0.0" {
		t.Fatalf("release.Version = %q, want 2.0.0", release.Version)
	}
	if release.Digest != digest {
		t.Fatalf("release.Digest = %q, want %q", release.Digest, digest)
	}
	if release.DownloadURL != server.URL+"/binary" {
		t.Fatalf("release.DownloadURL = %q, want %q", release.DownloadURL, server.URL+"/binary")
	}
}

func TestManagerCheckRejectsMissingDigestForUpdate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(VersionHeaderName, "2.0.0")
		w.Header().Set("Location", "/binary")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	manager := NewManager(server.URL+"/download", "1.2.3")
	release, err := manager.Check(context.Background())
	if err == nil {
		t.Fatal("Check() error = nil, want missing digest error")
	}
	if release != nil {
		t.Fatalf("Check() release = %+v, want nil", release)
	}
	if !strings.Contains(err.Error(), "missing digest") {
		t.Fatalf("Check() error = %v, want missing digest", err)
	}
}

func TestManagerDownloadReleaseAndPromote(t *testing.T) {
	binary := []byte("#!/bin/sh\nexit 0\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/binary":
			_, _ = w.Write(binary)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	currentPath := filepath.Join(dir, "hyperstack-agent")
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil { // #nosec G306 -- test executable path must be runnable.
		t.Fatalf("WriteFile(currentPath) error = %v", err)
	}

	manager := NewManager(server.URL+"/download", "1.0.0")
	release := &Release{
		Version:     "2.0.0",
		Digest:      digestFor(binary),
		DownloadURL: server.URL + "/binary",
	}

	if err := manager.DownloadRelease(context.Background(), release, currentPath); err != nil {
		t.Fatalf("DownloadRelease() error = %v", err)
	}
	if release.StagedPath == "" {
		t.Fatal("release.StagedPath = empty, want staged path")
	}

	if err := manager.PromoteRelease(currentPath, release); err != nil {
		t.Fatalf("PromoteRelease() error = %v", err)
	}

	got, err := os.ReadFile(currentPath) // #nosec G304 -- test path is created in t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(currentPath) error = %v", err)
	}
	if len(got) == 0 {
		t.Fatal("current binary is empty")
	}

	backup, err := os.ReadFile(currentPath + ".bak") // #nosec G304 -- test backup path is created by PromoteRelease.
	if err != nil {
		t.Fatalf("ReadFile(backup) error = %v", err)
	}
	if string(backup) != "old-binary" {
		t.Fatalf("backup binary = %q, want %q", string(backup), "old-binary")
	}

	if manager.CurrentVersion != "2.0.0" {
		t.Fatalf("CurrentVersion = %q, want 2.0.0", manager.CurrentVersion)
	}
}

func TestManagerDownloadReleaseRejectsMissingDigest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\nexit 0\n"))
	}))
	defer server.Close()

	dir := t.TempDir()
	currentPath := filepath.Join(dir, "hyperstack-agent")
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil { // #nosec G306 -- test executable path must be runnable.
		t.Fatalf("WriteFile(currentPath) error = %v", err)
	}

	manager := NewManager(server.URL+"/download", "1.0.0")
	release := &Release{
		Version:     "2.0.0",
		DownloadURL: server.URL,
	}

	if err := manager.DownloadRelease(context.Background(), release, currentPath); err == nil {
		t.Fatal("DownloadRelease() error = nil, want missing digest error")
	}
	if release.StagedPath != "" {
		t.Fatalf("release.StagedPath = %q, want empty", release.StagedPath)
	}
}

func TestManagerDownloadReleaseRejectsFailedSmokeTest(t *testing.T) {
	binary := []byte("#!/bin/sh\nexit 1\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(binary)
	}))
	defer server.Close()

	dir := t.TempDir()
	currentPath := filepath.Join(dir, "hyperstack-agent")
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil { // #nosec G306 -- test executable path must be runnable.
		t.Fatalf("WriteFile(currentPath) error = %v", err)
	}

	manager := NewManager(server.URL+"/download", "1.0.0")
	release := &Release{
		Version:     "2.0.0",
		Digest:      digestFor(binary),
		DownloadURL: server.URL,
	}

	if err := manager.DownloadRelease(context.Background(), release, currentPath); err == nil {
		t.Fatal("DownloadRelease() error = nil, want smoke test error")
	}
	if release.StagedPath != "" {
		t.Fatalf("release.StagedPath = %q, want empty", release.StagedPath)
	}
}

func TestManagerDownloadReleaseRejectsDigestMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\nexit 1\n"))
	}))
	defer server.Close()

	dir := t.TempDir()
	currentPath := filepath.Join(dir, "hyperstack-agent")
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil { // #nosec G306 -- test executable path must be runnable.
		t.Fatalf("WriteFile(currentPath) error = %v", err)
	}

	manager := NewManager(server.URL+"/download", "1.0.0")
	release := &Release{
		Version:     "2.0.0",
		Digest:      "sha256:deadbeef00000000000000000000000000000000000000000000000000000000",
		DownloadURL: server.URL,
	}

	if err := manager.DownloadRelease(context.Background(), release, currentPath); err == nil {
		t.Fatal("DownloadRelease() error = nil, want digest mismatch")
	}
	if release.StagedPath != "" {
		t.Fatalf("release.StagedPath = %q, want empty", release.StagedPath)
	}
}

func TestCopyWithLimitRejectsOversizedBinary(t *testing.T) {
	var dst bytes.Buffer
	err := copyWithLimit(&dst, bytes.NewBufferString("12345"), 4)
	if err == nil {
		t.Fatal("copyWithLimit() error = nil, want size error")
	}
	if dst.String() != "12345" {
		t.Fatalf("copied data = %q, want max+1 bytes", dst.String())
	}
}

func TestCopyWithLimitAcceptsExactLimit(t *testing.T) {
	var dst bytes.Buffer
	err := copyWithLimit(&dst, bytes.NewBufferString("1234"), 4)
	if err != nil {
		t.Fatalf("copyWithLimit() error = %v", err)
	}
	if dst.String() != "1234" {
		t.Fatalf("copied data = %q, want full data", dst.String())
	}
}

func digestFor(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum)
}
