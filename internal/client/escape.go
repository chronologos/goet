package client

// EscapeState tracks position within the ~. escape sequence.
type EscapeState int

const (
	escNone         EscapeState = iota // mid-line
	escAfterNewline                    // saw \r or \n (or connection start)
	escAfterTilde                      // saw ~ at start of line, held back
)

// EscapeAction is the result of processing input through the escape machine.
type EscapeAction int

const (
	EscSend       EscapeAction = iota // emit output bytes
	EscDisconnect                      // ~. detected
)

// EscapeProcessor detects the ~. escape sequence in terminal input.
// It holds back the ~ character when it appears at the start of a line,
// waiting to see if the next character completes an escape sequence.
type EscapeProcessor struct {
	state EscapeState
}

// NewEscapeProcessor creates an EscapeProcessor in the AfterNewline state,
// so ~. works immediately at connection start.
func NewEscapeProcessor() *EscapeProcessor {
	return &EscapeProcessor{state: escAfterNewline}
}

// Process runs input through the state machine, writing filtered output to dst.
// Returns (bytes written to dst, action). On EscDisconnect, the caller should
// discard any written bytes and initiate disconnection.
func (e *EscapeProcessor) Process(input, dst []byte) (int, EscapeAction) {
	n := 0
	for _, b := range input {
		switch e.state {
		case escNone:
			if b == '\r' || b == '\n' {
				e.state = escAfterNewline
			}
			dst[n] = b
			n++

		case escAfterNewline:
			switch {
			case b == '~':
				e.state = escAfterTilde
				// hold ~ — don't emit yet
			case b == '\r' || b == '\n':
				// stay in AfterNewline
				dst[n] = b
				n++
			default:
				e.state = escNone
				dst[n] = b
				n++
			}

		case escAfterTilde:
			switch {
			case b == '.':
				return n, EscDisconnect
			case b == '~':
				// ~~ → emit single ~
				e.state = escNone
				dst[n] = '~'
				n++
			case b == '\r' || b == '\n':
				// ~ followed by newline: emit both, back to AfterNewline
				e.state = escAfterNewline
				dst[n] = '~'
				n++
				dst[n] = b
				n++
			default:
				// ~ followed by non-special: emit both
				e.state = escNone
				dst[n] = '~'
				n++
				dst[n] = b
				n++
			}
		}
	}
	return n, EscSend
}

// Reset returns the processor to its initial state (AfterNewline).
func (e *EscapeProcessor) Reset() {
	e.state = escAfterNewline
}
