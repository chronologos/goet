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
	"strings"
	"syscall"

	"github.com/chronologos/goet/internal/client"
	"github.com/chronologos/goet/internal/session"
)

func main() {
	// Dispatch based on argv[0] (symlink name) or subcommand.
	base := filepath.Base(os.Args[0])

	switch {
	case base == "goet-session" || (len(os.Args) > 1 && os.Args[1] == "session"):
		runSession()
	case hasFlag("--local"):
		runClient()
	case hasDestination():
		runSSHClient()
	default:
		fmt.Fprintln(os.Stderr, "usage: goet [user@]host")
		fmt.Fprintln(os.Stderr, "       goet --local -p <port> -k <passkey-hex> [host]")
		fmt.Fprintln(os.Stderr, "       goet session -f <session-id> -p <port>")
		os.Exit(1)
	}
}

// hasFlag checks if a flag is present in os.Args.
func hasFlag(name string) bool {
	for _, arg := range os.Args[1:] {
		if arg == name {
			return true
		}
	}
	return false
}

// hasDestination checks if there's a non-flag argument that looks like a
// destination (hostname or user@host). Skips known flags like --profile.
func hasDestination() bool {
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "-") {
			return true
		}
	}
	return false
}

func runSSHClient() {
	profile := hasFlag("--profile")

	// First non-flag argument is the destination
	var destination string
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "-") {
			destination = arg
			break
		}
	}

	res, err := client.SpawnSSH(destination)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh: %v\n", err)
		os.Exit(1)
	}

	runClientWithConfig(client.Config{
		Host:    res.Host,
		Port:    res.Port,
		Passkey: res.Passkey,
		Profile: profile,
	})
}

func runClient() {
	profile := hasFlag("--profile")

	fs := flag.NewFlagSet("client", flag.ExitOnError)
	port := fs.Int("p", 0, "UDP port to connect to (required)")
	passkeyHex := fs.String("k", "", "hex-encoded passkey (required)")
	_ = fs.Bool("local", false, "direct mode (skip SSH)")

	// Filter out --local and --profile (flag package expects single-dash)
	var args []string
	for _, arg := range os.Args[1:] {
		if arg == "--local" || arg == "--profile" {
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
		Host:    host,
		Port:    *port,
		Passkey: passkey,
		Profile: profile,
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
	port := fs.Int("p", 0, "UDP port to listen on (0 = random)")

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
