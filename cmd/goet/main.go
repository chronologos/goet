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
	"strings"
	"syscall"

	"github.com/chronologos/goet/internal/client"
	"github.com/chronologos/goet/internal/session"
	"github.com/chronologos/goet/internal/transport"
)

func main() {
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
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "flags:")
			fmt.Fprintln(os.Stderr, "  --install   install goet on remote if missing")
			fmt.Fprintln(os.Stderr, "  --tcp       use TCP+TLS transport instead of QUIC")
			fmt.Fprintln(os.Stderr, "  --profile   emit RTT/traffic stats to stderr")
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

func runSSHClient(destination string) {
	profile := hasFlag("--profile")
	install := hasFlag("--install")
	mode := dialMode()

	res, err := client.SpawnSSH(destination, install)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh: %v\n", err)
		os.Exit(1)
	}

	runClientWithConfig(client.Config{
		Host:     res.Host,
		Port:     res.Port,
		Passkey:  res.Passkey,
		Profile:  profile,
		DialMode: mode,
	})
}

func runClient() {
	profile := hasFlag("--profile")
	mode := dialMode()

	fs := flag.NewFlagSet("client", flag.ExitOnError)
	port := fs.Int("p", 0, "port to connect to (required)")
	passkeyHex := fs.String("k", "", "hex-encoded passkey (required)")

	// Strip double-dash flags before parsing (flag package only supports single-dash).
	var args []string
	for _, arg := range os.Args[1:] {
		if arg == "--local" || arg == "--profile" || arg == "--install" || arg == "--tcp" {
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
		Host:     host,
		Port:     *port,
		Passkey:  passkey,
		Profile:  profile,
		DialMode: mode,
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

	cfg := session.Config{
		SessionID: *sessionID,
		Port:      *port,
		Passkey:   passkey,
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
