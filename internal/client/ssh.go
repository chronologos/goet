package client

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/chronologos/goet/internal/auth"
)

const sshTimeout = 10 * time.Second

// SSHResult holds the credentials and connection info obtained from the SSH
// bootstrapping phase. The SSH process is killed after the port is read.
type SSHResult struct {
	Host    string
	Port    int
	Passkey []byte
}

// sshArgs builds the common SSH argument prefix: [-l user] -o BatchMode=yes host.
func sshArgs(user, host string) []string {
	var args []string
	if user != "" {
		args = append(args, "-l", user)
	}
	return append(args, "-o", "BatchMode=yes", host)
}

// SpawnSSH launches an SSH process to start a goet session on the remote host.
// It generates a random passkey, sends it via SSH stdin, and reads the QUIC
// port from SSH stdout. The SSH process is killed once the port is obtained —
// all subsequent I/O flows over QUIC.
//
// If install is true and goet is not found on the remote, it installs goet
// automatically and retries with the absolute path ~/.local/bin/goet.
func SpawnSSH(destination string, install bool) (*SSHResult, error) {
	user, host := parseDestination(destination)

	// First attempt: try with "goet" in PATH
	res, err := spawnSSH(user, host, "goet")
	if err == nil {
		return res, nil
	}

	// If the error isn't "not installed", return as-is
	if !isNotInstalledError(err) {
		return nil, err
	}

	// goet is not installed on remote
	if !install {
		return nil, fmt.Errorf("goet is not installed on %s\n  Run: goet --install %s", host, destination)
	}

	// Install and retry
	slog.Info("goet not found on remote, installing...", "host", host)
	if err := installRemote(user, host); err != nil {
		return nil, fmt.Errorf("install goet on remote: %w", err)
	}

	slog.Info("retrying with absolute path", "path", remoteGoetPath)
	res, err = spawnSSH(user, host, remoteGoetPath)
	if err != nil {
		return nil, fmt.Errorf("connect after install: %w", err)
	}

	fmt.Fprintf(os.Stderr, "note: goet was installed to %s on %s\n", remoteGoetPath, host)
	fmt.Fprintf(os.Stderr, "      add ~/.local/bin to PATH for bare 'goet' invocations\n")
	return res, nil
}

// spawnSSH launches an SSH process to start a goet session on the remote host
// using the specified goet binary path.
func spawnSSH(user, host, goetPath string) (*SSHResult, error) {
	passkey, err := auth.GeneratePasskey()
	if err != nil {
		return nil, fmt.Errorf("generate passkey: %w", err)
	}

	passkeyHex := hex.EncodeToString(passkey)

	// No -t flag — PTY would corrupt the passkey/port protocol on stdin/stdout.
	args := append(sshArgs(user, host), goetPath, "session", "-f", "ssh", "-p", "0")

	cmd := exec.Command("ssh", args...)
	cmd.Stderr = os.Stderr // SSH errors (host key, auth failures) visible to user

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh: %w", err)
	}

	// Send passkey to session via SSH stdin, then close the write end
	// so the session sees EOF after reading the passkey line.
	if _, err := fmt.Fprintf(stdinPipe, "%s\n", passkeyHex); err != nil {
		killAndReap(cmd)
		return nil, fmt.Errorf("write passkey to ssh stdin: %w", err)
	}
	stdinPipe.Close()

	// Read port from session's stdout (via SSH). The session prints the port
	// once its QUIC listener is bound.
	port, err := readPort(stdoutPipe)
	if err != nil {
		killAndReap(cmd)
		return nil, err
	}

	// SSH is no longer needed — kill it. All data flows over QUIC from here.
	killAndReap(cmd)

	return &SSHResult{
		Host:    host,
		Port:    port,
		Passkey: passkey,
	}, nil
}

// readPort reads the port number from the SSH stdout pipe with a timeout.
// The session writes the port as a single line to stdout once its listener is ready.
func readPort(stdout io.Reader) (int, error) {
	type result struct {
		port int
		err  error
	}

	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			port, err := strconv.Atoi(line)
			if err != nil {
				ch <- result{err: fmt.Errorf("invalid port from session: %q", line)}
				return
			}
			ch <- result{port: port}
			return
		}
		// stdout closed before we got a line
		if err := scanner.Err(); err != nil {
			ch <- result{err: fmt.Errorf("read ssh stdout: %w", err)}
		} else {
			// EOF without a line — SSH or session exited early
			ch <- result{err: fmt.Errorf("ssh stdout closed before port received: %w", errNotInstalled)}
		}
	}()

	select {
	case res := <-ch:
		return res.port, res.err
	case <-time.After(sshTimeout):
		return 0, fmt.Errorf("timeout (%v) waiting for session port from ssh", sshTimeout)
	}
}

// killAndReap kills the process and waits for it to be reaped.
func killAndReap(cmd *exec.Cmd) {
	cmd.Process.Kill()
	cmd.Wait()
}

// parseDestination splits "[user@]host" into user and host.
// If no user is specified, user is empty.
func parseDestination(dest string) (user, host string) {
	if i := strings.LastIndex(dest, "@"); i >= 0 {
		return dest[:i], dest[i+1:]
	}
	return "", dest
}
