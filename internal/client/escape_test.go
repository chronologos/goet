package client

import "testing"

func TestEscapeProcessor(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   string
		action EscapeAction
	}{
		{
			name:   "normal passthrough",
			input:  "hello world",
			want:   "hello world",
			action: EscSend,
		},
		{
			name:   "newline then tilde dot disconnects",
			input:  "\n~.",
			want:   "\n",
			action: EscDisconnect,
		},
		{
			name:   "carriage return then tilde dot disconnects",
			input:  "\r~.",
			want:   "\r",
			action: EscDisconnect,
		},
		{
			name:   "tilde dot at connection start disconnects",
			input:  "~.",
			want:   "",
			action: EscDisconnect,
		},
		{
			name:   "double tilde emits single tilde",
			input:  "\n~~",
			want:   "\n~",
			action: EscSend,
		},
		{
			name:   "tilde mid-line is passed through",
			input:  "a~.",
			want:   "a~.",
			action: EscSend,
		},
		{
			name:   "tilde then non-special emits both",
			input:  "\n~x",
			want:   "\n~x",
			action: EscSend,
		},
		{
			name:   "tilde then newline emits tilde and newline",
			input:  "\n~\n",
			want:   "\n~\n",
			action: EscSend,
		},
		{
			name:   "consecutive newlines",
			input:  "\n\n\n",
			want:   "\n\n\n",
			action: EscSend,
		},
		{
			name:   "escape in multi-byte input",
			input:  "hello\r\n~.",
			want:   "hello\r\n",
			action: EscDisconnect,
		},
		{
			name:   "tilde at end of input is held",
			input:  "\n~",
			want:   "\n",
			action: EscSend,
		},
		{
			name:   "empty input",
			input:  "",
			want:   "",
			action: EscSend,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewEscapeProcessor()
			dst := make([]byte, len(tt.input)+2) // extra space for held ~
			n, action := e.Process([]byte(tt.input), dst)
			got := string(dst[:n])
			if got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
			if action != tt.action {
				t.Errorf("action = %d, want %d", action, tt.action)
			}
		})
	}
}

func TestEscapeProcessorMultiStep(t *testing.T) {
	// Simulate escape sequence split across multiple Process calls.
	e := NewEscapeProcessor()
	dst := make([]byte, 64)

	// First call: send "hello\n"
	n, action := e.Process([]byte("hello\n"), dst)
	if string(dst[:n]) != "hello\n" || action != EscSend {
		t.Fatalf("step 1: got %q action=%d", string(dst[:n]), action)
	}

	// Second call: send "~" — held back
	n, action = e.Process([]byte("~"), dst)
	if n != 0 || action != EscSend {
		t.Fatalf("step 2: got n=%d action=%d, want n=0 action=EscSend", n, action)
	}

	// Third call: send "." — completes escape
	n, action = e.Process([]byte("."), dst)
	if action != EscDisconnect {
		t.Fatalf("step 3: got action=%d, want EscDisconnect", action)
	}
}

func TestEscapeProcessorReset(t *testing.T) {
	e := NewEscapeProcessor()
	dst := make([]byte, 64)

	// Put processor mid-line
	e.Process([]byte("hello"), dst)

	// Reset should return to AfterNewline state
	e.Reset()

	// Now ~. should work
	_, action := e.Process([]byte("~."), dst)
	if action != EscDisconnect {
		t.Fatalf("after Reset, ~. should disconnect, got action=%d", action)
	}
}

func TestEscapeProcessorDoubleTildeFollowedByDot(t *testing.T) {
	// ~~. should emit ~ and . (not disconnect)
	e := NewEscapeProcessor()
	dst := make([]byte, 64)

	n, action := e.Process([]byte("~~."), dst)
	if action != EscSend {
		t.Fatalf("action = %d, want EscSend", action)
	}
	// Initial state is AfterNewline, so first ~ is held.
	// Second ~ triggers double-tilde: emit single ~, move to escNone.
	// Then . is mid-line: passthrough.
	if string(dst[:n]) != "~." {
		t.Fatalf("output = %q, want %q", string(dst[:n]), "~.")
	}
}
