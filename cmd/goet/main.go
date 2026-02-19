package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/chronologos/goet/internal/client"
	"github.com/chronologos/goet/internal/session"
	"github.com/chronologos/goet/internal/transport"
	"github.com/chronologos/goet/internal/version"
)

func main() {
	// Early flags that don't need dispatch.
	if hasFlag("--version") || (len(os.Args) > 1 && os.Args[1] == "version") {
		fmt.Printf("goet %s (%s)\n", version.VERSION, version.Commit)
		os.Exit(0)
	}

	// Dispatch based on argv[0] (symlink name) or subcommand.
	base := filepath.Base(os.Args[0])

	switch {
	case base == "goet-session" || (len(os.Args) > 1 && os.Args[1] == "session"):
		runSession()
	case hasFlag("--local"):
		runClient()
	default:
		if dest := findDestination(); dest != "" {
			runSSHClient(dest)
		} else {
			fmt.Fprintln(os.Stderr, "usage: goet [--install] [--tcp] [user@]host")
			fmt.Fprintln(os.Stderr, "       goet --local -p <port> -k <passkey-hex> [host]")
			fmt.Fprintln(os.Stderr, "       goet session -f <session-id> -p <port>")
			fmt.Fprintln(os.Stderr, "       goet version")
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "flags:")
			fmt.Fprintln(os.Stderr, "  --version              print version and exit")
			fmt.Fprintln(os.Stderr, "  --install              install or upgrade goet on remote")
			fmt.Fprintln(os.Stderr, "  --tcp                  use TCP+TLS transport instead of QUIC")
			fmt.Fprintln(os.Stderr, "  --profile              emit RTT/traffic stats to stderr")
			fmt.Fprintln(os.Stderr, "  --replay-size <bytes>  reconnect replay buffer size (default: 4096)")
			os.Exit(1)
		}
	}
}

// hasFlag checks if a flag is present in os.Args.
func hasFlag(name string) bool {
	return slices.Contains(os.Args[1:], name)
}

// findDestination returns the first non-flag argument (hostname or user@host),
// or "" if none found.
func findDestination() string {
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
}

// dialMode returns the transport mode based on CLI flags.
func dialMode() transport.DialMode {
	if hasFlag("--tcp") {
		return transport.DialTCP
	}
	return transport.DialQUIC
}

// replaySize returns the --replay-size value from CLI flags, or 0 for default.
// Supports both --replay-size <N> and --replay-size=<N> forms.
func replaySize() int {
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if arg == "--replay-size" && i+1 < len(os.Args) {
			if n, err := strconv.Atoi(os.Args[i+1]); err == nil {
				return n
			}
		}
		if v, ok := strings.CutPrefix(arg, "--replay-size="); ok {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return 0
}

func runSSHClient(destination string) {
	profile := hasFlag("--profile")
	install := hasFlag("--install")
	mode := dialMode()
	replay := replaySize()

	res, err := client.SpawnSSH(destination, install, replay)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "session %s (log: /tmp/goet-%s.log on remote)\n", res.SessionID, res.SessionID)

	runClientWithConfig(client.Config{
		Host:       res.Host,
		Port:       res.Port,
		Passkey:    res.Passkey,
		Profile:    profile,
		DialMode:   mode,
		ReplaySize: replay,
	})
}

func runClient() {
	profile := hasFlag("--profile")
	mode := dialMode()
	replay := replaySize()

	fs := flag.NewFlagSet("client", flag.ExitOnError)
	port := fs.Int("p", 0, "port to connect to (required)")
	passkeyHex := fs.String("k", "", "hex-encoded passkey (required)")

	// Strip double-dash flags before parsing (flag package only supports single-dash).
	var args []string
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch {
		case arg == "--local", arg == "--profile", arg == "--install", arg == "--tcp":
			continue
		case arg == "--replay-size" && i+1 < len(os.Args):
			i++ // skip value
			continue
		case strings.HasPrefix(arg, "--replay-size="):
			continue
		}
		args = append(args, arg)
	}
	fs.Parse(args)

	if *port == 0 {
		fmt.Fprintln(os.Stderr, "error: -p <port> is required")
		fs.Usage()
		os.Exit(1)
	}
	if *passkeyHex == "" {
		fmt.Fprintln(os.Stderr, "error: -k <passkey-hex> is required")
		fs.Usage()
		os.Exit(1)
	}

	passkey, err := hex.DecodeString(*passkeyHex)
	if err != nil || len(passkey) != 32 {
		fmt.Fprintln(os.Stderr, "error: passkey must be 64 hex characters (32 bytes)")
		os.Exit(1)
	}

	host := "127.0.0.1"
	if fs.NArg() > 0 {
		host = fs.Arg(0)
	}

	runClientWithConfig(client.Config{
		Host:       host,
		Port:       *port,
		Passkey:    passkey,
		Profile:    profile,
		DialMode:   mode,
		ReplaySize: replay,
	})
}

// runClientWithConfig creates a client with the given config and runs it
// until exit. Handles signal setup and error reporting.
func runClientWithConfig(cfg client.Config) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := client.New(cfg)
	if err := c.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "client exited: %v\n", err)
		os.Exit(1)
	}
}

func runSession() {
	fs := flag.NewFlagSet("session", flag.ExitOnError)
	sessionID := fs.String("f", "", "session ID (required, foreground mode)")
	port := fs.Int("p", 0, "port to listen on (0 = random)")
	replay := fs.Int("replay-size", 0, "catchup buffer size in bytes (0 = default 4KB)")

	// Skip "session" subcommand if present
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "session" {
		args = args[1:]
	}
	fs.Parse(args)

	if *sessionID == "" {
		fmt.Fprintln(os.Stderr, "error: -f <session-id> is required")
		fs.Usage()
		os.Exit(1)
	}

	// Read hex-encoded passkey from stdin (64 hex chars = 32 bytes)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading passkey from stdin: %v\n", err)
		os.Exit(1)
	}
	line = strings.TrimSpace(line)

	passkey, err := hex.DecodeString(line)
	if err != nil || len(passkey) != 32 {
		fmt.Fprintln(os.Stderr, "error: passkey must be 64 hex characters (32 bytes)")
		os.Exit(1)
	}

	// Log to /tmp/goet-<id>.log so session diagnostics survive SSH teardown.
	logPath := fmt.Sprintf("/tmp/goet-%s.log", *sessionID)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open log file %s: %v\n", logPath, err)
	}
	if logFile != nil {
		defer logFile.Close()
	}

	cfg := session.Config{
		SessionID:  *sessionID,
		Port:       *port,
		Passkey:    passkey,
		LogWriter:  logFile, // nil falls back to stderr in session.New()
		ReplaySize: *replay,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s := session.New(cfg)

	// Print port once the listener is ready (for parent processes / scripts)
	go func() {
		<-s.Ready
		fmt.Println(s.Port)
	}()

	if err := s.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "session exited: %v\n", err)
		os.Exit(1)
	}
}
