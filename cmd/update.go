package cmd

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/output"
)

const (
	updateLatestAPI      = "https://api.github.com/repos/tszaks/pallium/releases/latest"
	updateReleaseBase    = "https://github.com/tszaks/pallium/releases/download"
	updateModulePath     = "github.com/tszaks/pallium"
	updateRequestTimeout = 2 * time.Minute
)

// Test seams: these let update tests fake the release API, the download
// mirror, the running executable's path, the platform, and the npm shell-out.
var (
	updateHTTPClient     = &http.Client{Timeout: updateRequestTimeout}
	updateLatestURL      = updateLatestAPI
	updateDownloadBase   = updateReleaseBase
	updateExecutableFunc = os.Executable
	updateEvalSymlinks   = filepath.EvalSymlinks
	updateUserHomeDir    = os.UserHomeDir
	updateLookPath       = exec.LookPath
	updateNPMInstall     = runNPMUpdateInstall
	updateGOOS           = runtime.GOOS
	updateGOARCH         = runtime.GOARCH
)

type UpdateReport struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	Updated        bool   `json:"updated"`
	Method         string `json:"method,omitempty"`
	Executable     string `json:"executable,omitempty"`
	Message        string `json:"message"`
}

func runUpdate(out io.Writer, jsonOutput bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), updateRequestTimeout)
	defer cancel()

	current := currentVersion()
	latest, err := latestReleaseTag(ctx)
	if err != nil {
		return fmt.Errorf("check latest release: %w", err)
	}
	report := UpdateReport{CurrentVersion: current, LatestVersion: latest}

	write := func() error {
		return output.Write(out, report, jsonOutput, func() string { return report.Message })
	}
	if sameReleaseVersion(current, latest) {
		report.Message = fmt.Sprintf("pallium is already up to date (%s)", current)
		return write()
	}

	exe, err := updateExecutableFunc()
	if err == nil {
		if resolved, resolveErr := updateEvalSymlinks(exe); resolveErr == nil {
			exe = resolved
		}
		report.Executable = exe
	}

	// npm installs keep the binary under ~/.pallium/npm/v<tag>/ and the node
	// wrapper resolves that path from the package's own version — so the
	// canonical update is `npm install -g pallium@latest`, which bumps the
	// package and lets the wrapper download the new release. Only fall back
	// to swapping the binary in place when npm itself is not on PATH.
	if err == nil && isNPMManagedExecutable(exe) {
		if _, lookErr := updateLookPath("npm"); lookErr == nil {
			if installErr := updateNPMInstall(ctx); installErr != nil {
				return fmt.Errorf("npm install -g pallium@latest failed: %w", installErr)
			}
			report.Updated = true
			report.Method = "npm"
			report.Message = fmt.Sprintf("updated pallium %s -> %s via npm", current, latest)
			return write()
		}
	}

	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if err := swapExecutable(ctx, latest, exe); err != nil {
		return err
	}
	report.Updated = true
	report.Method = "binary-swap"
	report.Message = fmt.Sprintf("updated pallium %s -> %s (%s)", current, latest, exe)
	return write()
}

func currentVersion() string {
	version := buildVersion
	if version == "dev" || version == "" {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
	}
	if version == "" {
		return "dev"
	}
	return version
}

func sameReleaseVersion(current, latest string) bool {
	current = strings.TrimSpace(current)
	if current == "" || current == "dev" {
		return false
	}
	return strings.TrimPrefix(current, "v") == strings.TrimPrefix(strings.TrimSpace(latest), "v")
}

func latestReleaseTag(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, updateLatestURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "pallium/"+currentVersion())
	response, err := updateHTTPClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release check returned %s", response.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode release response: %w", err)
	}
	if body.TagName == "" {
		return "", fmt.Errorf("release response had no tag_name")
	}
	return body.TagName, nil
}

func isNPMManagedExecutable(exe string) bool {
	home, err := updateUserHomeDir()
	if err != nil || home == "" {
		return false
	}
	// The executable is resolved through symlinks by the caller, so the home
	// prefix must be too — e.g. macOS maps /var -> /private/var, and a
	// symlinked $HOME would otherwise never match.
	if resolved, resolveErr := updateEvalSymlinks(home); resolveErr == nil {
		home = resolved
	}
	prefix := filepath.Join(home, ".pallium", "npm") + string(os.PathSeparator)
	return strings.HasPrefix(exe, prefix)
}

func runNPMUpdateInstall(ctx context.Context) error {
	command := exec.CommandContext(ctx, "npm", "install", "-g", "pallium@latest")
	outputBytes, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(outputBytes)))
	}
	return nil
}

func updateAssetName(tag string) (string, error) {
	version := strings.TrimPrefix(tag, "v")
	platform := updateGOOS + "-" + updateGOARCH
	switch platform {
	case "darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64":
		return fmt.Sprintf("pallium_%s_%s_%s.tar.gz", version, updateGOOS, updateGOARCH), nil
	}
	return "", fmt.Errorf("no prebuilt binary for %s; reinstall with `npm install -g pallium@latest` or `go install %s@%s`", platform, updateModulePath, tag)
}

func swapExecutable(ctx context.Context, tag, exe string) error {
	asset, err := updateAssetName(tag)
	if err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "pallium-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	base := strings.TrimRight(updateDownloadBase, "/") + "/" + tag
	archivePath := filepath.Join(tmpDir, asset)
	if err := downloadReleaseFile(ctx, base+"/"+asset, archivePath); err != nil {
		return fmt.Errorf("download %s: %w", asset, err)
	}
	checksumsPath := filepath.Join(tmpDir, "checksums.txt")
	if err := downloadReleaseFile(ctx, base+"/checksums.txt", checksumsPath); err != nil {
		return fmt.Errorf("download checksums.txt: %w", err)
	}
	if err := verifyReleaseChecksum(archivePath, checksumsPath, asset); err != nil {
		return err
	}
	staged, err := extractReleaseBinary(archivePath, filepath.Dir(exe))
	if err != nil {
		return err
	}
	if err := os.Rename(staged, exe); err != nil {
		return fmt.Errorf("replace %s: %w (try `npm install -g pallium@latest` or reinstall with sufficient permissions)", exe, err)
	}
	return nil
}

func downloadReleaseFile(ctx context.Context, url, destination string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "pallium/"+currentVersion())
	request.Header.Set("Accept", "application/octet-stream")
	response, err := updateHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s", url, response.Status)
	}
	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, response.Body)
	return err
}

func verifyReleaseChecksum(archivePath, checksumsPath, asset string) error {
	contents, err := os.ReadFile(checksumsPath)
	if err != nil {
		return err
	}
	var expected string
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			expected = fields[0]
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("checksums.txt did not include %s", asset)
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, archive); err != nil {
		return err
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != expected {
		return fmt.Errorf("checksum mismatch for %s", asset)
	}
	return nil
}

// extractReleaseBinary unpacks the release tarball, copies the `pallium`
// binary into a staged file next to the install target, and returns that
// path — the caller os.Rename()s it over the executable so the swap is
// atomic on the same filesystem.
func extractReleaseBinary(archivePath, stageDir string) (string, error) {
	archive, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	reader, err := gzip.NewReader(archive)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.Base(archivePath), err)
	}
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return "", fmt.Errorf("archive did not contain the pallium binary")
		}
		if err != nil {
			return "", err
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != "pallium" {
			continue
		}
		staged, err := os.CreateTemp(stageDir, ".pallium-update-*")
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(staged, tarReader); err != nil {
			staged.Close()
			os.Remove(staged.Name())
			return "", err
		}
		if err := staged.Close(); err != nil {
			os.Remove(staged.Name())
			return "", err
		}
		if err := os.Chmod(staged.Name(), 0o755); err != nil {
			os.Remove(staged.Name())
			return "", err
		}
		return staged.Name(), nil
	}
}
