package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/creack/pty"
)

// spawnPTY starts a shell in a new PTY with 24x80 default size.
// Returns the PTY master file and the running command.
// Shell is $SHELL or /bin/sh. Sets GOET_SESSION env var.
func spawnPTY(sessionID string) (*os.File, *exec.Cmd, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	cmd := exec.Command(shell)

	// Login shell: prepend "-" to argv[0] so the shell reads profile files.
	shellBase := filepath.Base(shell)
	cmd.Args[0] = "-" + shellBase

	env := os.Environ()
	// Ensure TERM is set — SSH without -t doesn't provide one,
	// and many programs (clear, less, vim) need it.
	if os.Getenv("TERM") == "" {
		env = append(env, "TERM=xterm-256color")
	}
	cmd.Env = append(env, "GOET_SESSION="+sessionID)

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return nil, nil, fmt.Errorf("start PTY: %w", err)
	}

	return ptmx, cmd, nil
}

// resizePTY sets the PTY to the given dimensions.
func resizePTY(ptmx *os.File, rows, cols uint16) error {
	return pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// bouncePTYSize shrinks the PTY by 1 row then restores nothing — the client
// sends its real size immediately after reconnect, which will be different
// from the shrunk size, guaranteeing a genuine size change → SIGWINCH → TUI
// apps redraw. If the PTY is already 1 row, this is a no-op.
func bouncePTYSize(ptmx *os.File) error {
	sz, err := pty.GetsizeFull(ptmx)
	if err != nil {
		return fmt.Errorf("get PTY size: %w", err)
	}
	if sz.Rows <= 1 {
		return nil
	}
	sz.Rows--
	return pty.Setsize(ptmx, sz)
}
