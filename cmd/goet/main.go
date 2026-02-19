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
	"strconv"
	"strings"
	"syscall"

	"github.com/chronologos/goet/internal/client"
	"github.com/chronologos/goet/internal/session"
	"github.com/chronologos/goet/internal/transport"
	"github.com/chronologos/goet/internal/version"
)

// globalFlags holds double-dash flags parsed from os.Args before dispatch.
// rest contains the remaining arguments with global flags stripped.
type globalFlags struct {
	version    bool
	local      bool
	install    bool
	tcp        bool
	profile    bool
	replaySize int
	rest       []string
}

func (g globalFlags) dialMode() transport.DialMode {
	if g.tcp {
		return transport.DialTCP
	}
	return transport.DialQUIC
}

// parseGlobalFlags extracts double-dash flags from os.Args and returns
// the parsed values plus remaining args. Supports --flag and --flag=value forms.
func parseGlobalFlags() globalFlags {
	var g globalFlags
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch {
		case arg == "--version":
			g.version = true
		case arg == "--local":
			g.local = true
		case arg == "--install":
			g.install = true
		case arg == "--tcp":
			g.tcp = true
		case arg == "--profile":
			g.profile = true
		case arg == "--replay-size" && i+1 < len(os.Args):
			i++
			g.replaySize, _ = strconv.Atoi(os.Args[i])
		case strings.HasPrefix(arg, "--replay-size="):
			v, _ := strings.CutPrefix(arg, "--replay-size=")
			g.replaySize, _ = strconv.Atoi(v)
		default:
			g.rest = append(g.rest, arg)
		}
	}
	return g
}

func main() {
	gf := parseGlobalFlags()

	if gf.version || (len(gf.rest) > 0 && gf.rest[0] == "version") {
		fmt.Printf("goet %s (%s)\n", version.VERSION, version.Commit)
		os.Exit(0)
	}

	base := filepath.Base(os.Args[0])

	switch {
	case base == "goet-session" || (len(gf.rest) > 0 && gf.rest[0] == "session"):
		runSession(gf.rest)
	case gf.local:
		runClient(gf)
	default:
		if dest := findDestination(gf.rest); dest != "" {
			runSSHClient(dest, gf)
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

// findDestination returns the first non-flag argument (hostname or user@host),
// or "" if none found. args should have global flags already stripped.
func findDestination(args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
}

func runSSHClient(destination string, gf globalFlags) {
	res, err := client.SpawnSSH(destination, gf.install, gf.replaySize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "session %s (log: /tmp/goet-%s.log on remote)\n", res.SessionID, res.SessionID)

	runClientWithConfig(client.Config{
		Host:       res.Host,
		Port:       res.Port,
		Passkey:    res.Passkey,
		Profile:    gf.profile,
		DialMode:   gf.dialMode(),
		ReplaySize: gf.replaySize,
	})
}

func runClient(gf globalFlags) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	port := fs.Int("p", 0, "port to connect to (required)")
	passkeyHex := fs.String("k", "", "hex-encoded passkey (required)")
	fs.Parse(gf.rest)

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
		Profile:    gf.profile,
		DialMode:   gf.dialMode(),
		ReplaySize: gf.replaySize,
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

func runSession(args []string) {
	fs := flag.NewFlagSet("session", flag.ExitOnError)
	sessionID := fs.String("f", "", "session ID (required, foreground mode)")
	port := fs.Int("p", 0, "port to listen on (0 = random)")
	replay := fs.Int("replay-size", 0, "catchup buffer size in bytes (0 = default 4KB)")

	// Skip "session" subcommand if present
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
