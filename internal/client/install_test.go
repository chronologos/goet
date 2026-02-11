package client

import (
	"fmt"
	"testing"
)

func TestParseUnameOutput(t *testing.T) {
	tests := []struct {
		input   string
		wantOS  string
		wantArch string
		wantErr bool
	}{
		// Standard cases
		{"Linux x86_64", "linux", "amd64", false},
		{"Linux aarch64", "linux", "arm64", false},
		{"Linux arm64", "linux", "arm64", false},
		{"Darwin x86_64", "darwin", "amd64", false},
		{"Darwin arm64", "darwin", "arm64", false},

		// Error cases
		{"FreeBSD amd64", "", "", true},      // unsupported OS
		{"Linux mips", "", "", true},          // unsupported arch
		{"Linux", "", "", true},               // missing arch
		{"", "", "", true},                    // empty
		{"Linux x86_64 extra", "", "", true},  // too many fields
		{"Windows x86_64", "", "", true},      // unsupported OS
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			goos, goarch, err := parseUnameOutput(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseUnameOutput(%q) = (%q, %q, nil), want error", tt.input, goos, goarch)
				}
				return
			}
			if err != nil {
				t.Errorf("parseUnameOutput(%q) error: %v", tt.input, err)
				return
			}
			if goos != tt.wantOS || goarch != tt.wantArch {
				t.Errorf("parseUnameOutput(%q) = (%q, %q), want (%q, %q)", tt.input, goos, goarch, tt.wantOS, tt.wantArch)
			}
		})
	}
}

func TestIsNotInstalledError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errNotInstalled, true},
		{fmt.Errorf("ssh stdout closed before port received: %w", errNotInstalled), true},
		{fmt.Errorf("timeout (10s) waiting for session port from ssh"), false},
		{fmt.Errorf("start ssh: exec: not found"), false},
		{fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", errNotInstalled)), true},
	}

	for _, tt := range tests {
		name := "<nil>"
		if tt.err != nil {
			name = tt.err.Error()
		}
		t.Run(name, func(t *testing.T) {
			got := isNotInstalledError(tt.err)
			if got != tt.want {
				t.Errorf("isNotInstalledError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
