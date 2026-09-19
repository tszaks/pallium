package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateEnv stubs every update seam around a test and returns a fake release
// server (latest-tag API plus the asset/checksum download endpoints).
type updateEnv struct {
	home            string
	server          *httptest.Server
	npmInstallRan   bool
	corruptChecksum bool
}

func stubUpdate(t *testing.T, tag string, binary []byte) *updateEnv {
	t.Helper()
	tmp := t.TempDir()
	env := &updateEnv{home: tmp}

	tarball := updateTestTarball(t, tag, binary)
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q}`, tag)
	})
	mux.HandleFunc("/"+tag+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		sum := sha256.Sum256(tarball)
		if env.corruptChecksum {
			sum = sha256.Sum256([]byte("tampered"))
		}
		fmt.Fprintf(w, "%x  %s\n", sum, updateTestAssetName(tag))
	})
	mux.HandleFunc("/"+tag+"/"+updateTestAssetName(tag), func(w http.ResponseWriter, _ *http.Request) {
		w.Write(tarball)
	})
	env.server = httptest.NewServer(mux)
	t.Cleanup(env.server.Close)

	setVar(t, &updateLatestURL, env.server.URL+"/latest")
	setVar(t, &updateDownloadBase, env.server.URL)
	setVar(t, &updateUserHomeDir, func() (string, error) { return env.home, nil })
	setVar(t, &updateGOOS, "linux")
	setVar(t, &updateGOARCH, "amd64")
	setVar(t, &updateLookPath, func(string) (string, error) { return "", fmt.Errorf("not found") })
	setVar(t, &updateNPMInstall, func(context.Context) error {
		env.npmInstallRan = true
		return nil
	})
	return env
}

func updateTestAssetName(tag string) string {
	return fmt.Sprintf("pallium_%s_linux_amd64.tar.gz", strings.TrimPrefix(tag, "v"))
}

func updateTestTarball(t *testing.T, tag string, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	name := fmt.Sprintf("pallium_%s_linux_amd64/pallium", strings.TrimPrefix(tag, "v"))
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func setVar[T any](t *testing.T, slot *T, value T) {
	t.Helper()
	original := *slot
	*slot = value
	t.Cleanup(func() { *slot = original })
}

func runUpdateReport(t *testing.T) UpdateReport {
	t.Helper()
	var out bytes.Buffer
	if err := runUpdate(&out, true); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	var report UpdateReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode report %q: %v", out.String(), err)
	}
	return report
}

func TestUpdateAlreadyLatest(t *testing.T) {
	setVar(t, &buildVersion, "v1.2.3")
	env := stubUpdate(t, "v1.2.3", []byte("new-binary"))
	report := runUpdateReport(t)
	if report.Updated || report.Method != "" {
		t.Fatalf("expected no update, got %+v", report)
	}
	if !strings.Contains(report.Message, "already up to date") {
		t.Fatalf("unexpected message %q", report.Message)
	}
	if env.npmInstallRan {
		t.Fatal("npm install ran for an up-to-date install")
	}
}

func TestUpdateNPMLayoutUsesNPM(t *testing.T) {
	setVar(t, &buildVersion, "v0.9.0")
	env := stubUpdate(t, "v9.9.9", []byte("new-binary"))
	exe := filepath.Join(env.home, ".pallium", "npm", "v0.9.0", "pallium")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	setVar(t, &updateExecutableFunc, func() (string, error) { return exe, nil })
	setVar(t, &updateLookPath, func(name string) (string, error) {
		return "/usr/local/bin/" + name, nil
	})
	report := runUpdateReport(t)
	if !report.Updated || report.Method != "npm" || !env.npmInstallRan {
		t.Fatalf("expected npm update, got %+v", report)
	}
	contents, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "old-binary" {
		t.Fatal("npm path should not touch the binary file")
	}
}

func TestUpdateNPMLayoutWithoutNPMFallsBackToSwap(t *testing.T) {
	setVar(t, &buildVersion, "v0.9.0")
	env := stubUpdate(t, "v1.2.3", []byte("new-binary"))
	exe := filepath.Join(env.home, ".pallium", "npm", "v0.9.0", "pallium")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	setVar(t, &updateExecutableFunc, func() (string, error) { return exe, nil })
	report := runUpdateReport(t)
	if !report.Updated || report.Method != "binary-swap" || env.npmInstallRan {
		t.Fatalf("expected binary-swap fallback, got %+v", report)
	}
	contents, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "new-binary" {
		t.Fatalf("binary not swapped, got %q", contents)
	}
}

func TestUpdateBinarySwapReplacesExecutable(t *testing.T) {
	setVar(t, &buildVersion, "v0.9.0")
	env := stubUpdate(t, "v1.2.3", []byte("new-binary-bytes"))
	exe := filepath.Join(env.home, "bin", "pallium")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	setVar(t, &updateExecutableFunc, func() (string, error) { return exe, nil })
	report := runUpdateReport(t)
	if !report.Updated || report.Method != "binary-swap" {
		t.Fatalf("expected binary-swap, got %+v", report)
	}
	contents, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "new-binary-bytes" {
		t.Fatalf("binary not swapped, got %q", contents)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("swapped binary lost executable bits")
	}
}

func TestUpdateChecksumMismatchLeavesBinaryAlone(t *testing.T) {
	setVar(t, &buildVersion, "v0.9.0")
	env := stubUpdate(t, "v1.2.3", []byte("new-binary"))
	exe := filepath.Join(env.home, "pallium")
	if err := os.WriteFile(exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	setVar(t, &updateExecutableFunc, func() (string, error) { return exe, nil })
	env.corruptChecksum = true
	var out bytes.Buffer
	if err := runUpdate(&out, true); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
	contents, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "old-binary" {
		t.Fatal("binary was modified despite failure")
	}
}

func TestUpdateUnsupportedPlatform(t *testing.T) {
	setVar(t, &buildVersion, "v0.9.0")
	env := stubUpdate(t, "v1.2.3", []byte("new-binary"))
	exe := filepath.Join(env.home, "pallium")
	if err := os.WriteFile(exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	setVar(t, &updateExecutableFunc, func() (string, error) { return exe, nil })
	setVar(t, &updateGOOS, "windows")
	var out bytes.Buffer
	err := runUpdate(&out, true)
	if err == nil || !strings.Contains(err.Error(), "no prebuilt binary") {
		t.Fatalf("expected no-prebuilt error, got %v", err)
	}
}
