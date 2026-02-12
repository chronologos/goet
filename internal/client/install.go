package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	sshRunTimeout      = 30 * time.Second
	sshTransferTimeout = 60 * time.Second
	downloadTimeout    = 60 * time.Second
	// remoteGoetPath uses ~ which is expanded by the remote login shell.
	// Do not use in contexts without shell expansion (e.g., SFTP).
	remoteGoetPath = "~/.local/bin/goet"
	releaseURLBase = "https://github.com/chronologos/goet/releases/latest/download"
)

// errNotInstalled is a sentinel error indicating goet is not installed on the remote.
var errNotInstalled = errors.New("goet may not be installed on remote")

// sshRun executes a command on the remote host via SSH and returns combined output.
func sshRun(user, host, command string) (string, error) {
	args := append(sshArgs(user, host), command)

	ctx, cancel := context.WithTimeout(context.Background(), sshRunTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("ssh command %q: %w\n%s", command, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// detectRemoteArch runs `uname -sm` on remote, returns (goos, goarch).
func detectRemoteArch(user, host string) (goos, goarch string, err error) {
	out, err := sshRun(user, host, "uname -sm")
	if err != nil {
		return "", "", fmt.Errorf("detect remote arch: %w", err)
	}
	return parseUnameOutput(out)
}

// parseUnameOutput parses the output of `uname -sm` into (goos, goarch).
// Mapping: Linux→linux, Darwin→darwin; x86_64→amd64, aarch64|arm64→arm64.
func parseUnameOutput(output string) (goos, goarch string, err error) {
	fields := strings.Fields(output)
	if len(fields) != 2 {
		return "", "", fmt.Errorf("unexpected uname output: %q", output)
	}

	kernel, machine := fields[0], fields[1]

	switch kernel {
	case "Linux":
		goos = "linux"
	case "Darwin":
		goos = "darwin"
	default:
		return "", "", fmt.Errorf("unsupported OS: %q", kernel)
	}

	switch machine {
	case "x86_64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", fmt.Errorf("unsupported architecture: %q", machine)
	}

	return goos, goarch, nil
}

// installRemote detects remote arch, transfers the appropriate binary to
// ~/.local/bin/goet, and makes it executable. Uses self-transfer for same
// arch, GitHub release download for cross-arch.
func installRemote(user, host string) error {
	goos, goarch, err := detectRemoteArch(user, host)
	if err != nil {
		return err
	}

	slog.Info("detected remote architecture", "os", goos, "arch", goarch)

	sameArch := goos == runtime.GOOS && goarch == runtime.GOARCH
	if sameArch {
		binaryPath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("get local executable path: %w", err)
		}
		slog.Info("same architecture, self-transferring", "path", binaryPath)
		return transferBinary(user, host, binaryPath)
	}

	// Cross-architecture: download from GitHub releases
	slog.Info("cross-architecture, downloading release", "target", goos+"/"+goarch)
	binaryPath, cleanup, err := downloadRelease(goos, goarch)
	if err != nil {
		return err
	}
	defer cleanup()

	return transferBinary(user, host, binaryPath)
}

// transferBinary pipes a local binary to the remote host via SSH stdin,
// installing it at ~/.local/bin/goet.
func transferBinary(user, host, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open binary: %w", err)
	}
	defer f.Close()

	installCmd := fmt.Sprintf("mkdir -p ~/.local/bin && cat > %s && chmod +x %s", remoteGoetPath, remoteGoetPath)
	args := append(sshArgs(user, host), installCmd)

	ctx, cancel := context.WithTimeout(context.Background(), sshTransferTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = f
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("transfer binary to remote: %w", err)
	}

	slog.Info("installed goet on remote", "host", host, "path", remoteGoetPath)
	return nil
}

// downloadRelease fetches the goet binary for the given OS/arch from GitHub releases.
// Returns the path to a temp file, a cleanup function, and any error.
// URL pattern: https://github.com/chronologos/goet/releases/latest/download/goet-{os}-{arch}
func downloadRelease(goos, goarch string) (path string, cleanup func(), err error) {
	url := fmt.Sprintf("%s/goet-%s-%s", releaseURLBase, goos, goarch)

	httpClient := &http.Client{Timeout: downloadTimeout}
	resp, err := httpClient.Get(url)
	if err != nil {
		return "", nil, fmt.Errorf("download release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("download release: HTTP %d from %s", resp.StatusCode, url)
	}

	tmpFile, err := os.CreateTemp("", "goet-download-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", nil, fmt.Errorf("write release to temp file: %w", err)
	}
	tmpFile.Close()

	return tmpFile.Name(), func() { os.Remove(tmpFile.Name()) }, nil
}

// isNotInstalledError checks if an error from spawnSSH indicates that goet
// is not installed on the remote host.
func isNotInstalledError(err error) bool {
	return errors.Is(err, errNotInstalled)
}

// getRemoteVersion runs `goet --version` on the remote and returns the commit hash.
// Tries "goet" in PATH first, then the absolute install path.
// Returns errNotInstalled if goet is not found at either location.
func getRemoteVersion(user, host string) (string, error) {
	out, err := sshRun(user, host, "goet --version")
	if err == nil {
		return parseVersionOutput(out)
	}

	// Try absolute path (may have been installed by --install but not in PATH)
	out, err = sshRun(user, host, remoteGoetPath+" --version")
	if err != nil {
		return "", errNotInstalled
	}
	return parseVersionOutput(out)
}

// parseVersionOutput extracts the commit hash from "goet <commit>" output.
func parseVersionOutput(output string) (string, error) {
	fields := strings.Fields(output)
	if len(fields) != 2 || fields[0] != "goet" {
		return "", fmt.Errorf("unexpected version output: %q", output)
	}
	return fields[1], nil
}
