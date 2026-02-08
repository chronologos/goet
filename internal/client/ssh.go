package client

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
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

// SpawnSSH launches an SSH process to start a goet session on the remote host.
// It generates a random passkey and session ID, sends the passkey via SSH's
// stdin, and reads the port number from SSH's stdout. The SSH process is killed
// once the port is obtained — all subsequent I/O flows over QUIC.
//
// The remote host must have goet in its PATH.
func SpawnSSH(destination string) (*SSHResult, error) {
	user, host := parseDestination(destination)

	passkey, err := auth.GeneratePasskey()
	if err != nil {
		return nil, fmt.Errorf("generate passkey: %w", err)
	}

	passkeyHex := hex.EncodeToString(passkey)

	// Build SSH command: ssh [-l user] host goet session -f <id> -p 0
	// No -t flag — PTY would corrupt the passkey/port protocol on stdin/stdout.
	var args []string
	if user != "" {
		args = append(args, "-l", user)
	}
	// -o BatchMode=yes: fail fast if key auth isn't configured (don't prompt for password)
	args = append(args, "-o", "BatchMode=yes", host, "goet", "session", "-f", "ssh", "-p", "0")

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
			ch <- result{err: fmt.Errorf("ssh stdout closed before port received (goet may not be installed on remote)")}
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
