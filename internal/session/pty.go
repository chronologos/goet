package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/creack/pty"
)

// spawnPTY starts a shell in a new PTY with 24x80 default size.
// Returns the PTY master file and the running command.
// Shell is $SHELL or /bin/sh. Sets GOET_SESSION env var.
// The term parameter sets TERM in the shell environment; if empty,
// falls back to xterm-256color.
func spawnPTY(sessionID, term string) (*os.File, *exec.Cmd, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	cmd := exec.Command(shell)

	// Login shell: prepend "-" to argv[0] so the shell reads profile files.
	shellBase := filepath.Base(shell)
	cmd.Args[0] = "-" + shellBase

	term = sanitizeTerm(term)

	// Filter out any inherited TERM= and inject the client's value.
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TERM=") {
			env = append(env, e)
		}
	}
	env = append(env, "TERM="+term)
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

// sanitizeTerm validates a TERM value from the client. Returns the value if
// it looks reasonable, or "xterm-256color" as a safe fallback.
func sanitizeTerm(term string) string {
	if term == "" || len(term) > 128 {
		return "xterm-256color"
	}
	for _, c := range term {
		if c < 0x20 || c == '=' || c > 0x7e {
			return "xterm-256color"
		}
	}
	return term
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
