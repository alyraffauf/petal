package tui

import (
	"golang.org/x/sys/unix"
	"io"
	"os"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/term"
)

// The keyboard decoder times out incomplete escape sequences. A pause inside
// an SSH clipboard reply must never turn the remaining base64 into keystrokes.
// Only short markers pass through the keyboard decoder, preserving their
// order relative to keys and bracketed pastes in the same terminal read.
const clipboardPrefix = "\x1b]5522;"
const clipboardPacketMarker = "\x1b]9;p\a"
const maxClipboardPacket = 64 << 10

type clipboardInput struct {
	*os.File
	prefix      int
	sentEscape  bool
	packet      []byte
	oversized   bool
	lastEscape  bool
	bracketed   bool
	pastePrefix int
	mu          sync.Mutex
	packets     []tea.Msg
}
type oversizedClipboardPacket struct{}

func runChat(m *terminal) error {
	tty := os.Stdin
	if !term.IsTerminal(tty.Fd()) {
		var err error
		tty, err = os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer tty.Close()
	}
	m.inputTTY = tty
	input := &clipboardInput{File: tty}
	defer func() {
		if m.launch != nil {
			m.launch.stopWatchingChildStatuses()
			m.launch.stopTranscriptSearch()
		}
	}()
	_, err := tea.NewProgram(&frame{Model: m, background: canvasBackground(m.cfg.TransparentBackground)}, tea.WithInput(input), tea.WithFilter(input.restorePacket)).Run()
	return err
}

func (c *clipboardInput) Read(out []byte) (int, error) {
	// Ultraviolet supplies a 4096-byte read buffer. Leave room for a prefix
	// carried across reads and the completion marker for an earlier packet.
	var raw [2048]byte
	if len(out) < 2*len(clipboardPrefix)+1 {
		return 0, io.ErrShortBuffer
	}
	n, err := c.File.Read(raw[:min(len(raw), len(out)-2*len(clipboardPrefix))])
	framed := c.frame(out[:0], raw[:n])
	// Resolve a bare Escape on the same deadline as the keyboard decoder.
	// Keeping it here until then also lets a split OSC introducer stay intact.
	if c.prefix == 1 && !c.sentEscape {
		fd := []unix.PollFd{{Fd: int32(c.Fd()), Events: unix.POLLIN}}
		// A signal can interrupt poll while no further key is coming.
		// Retry until the original Escape deadline to avoid blocking in Read.
		deadline := time.Now().Add(uv.DefaultEscTimeout)
		var ready int
		var pollErr error
		for remaining := time.Until(deadline); remaining > 0; remaining = time.Until(deadline) {
			ready, pollErr = unix.Poll(fd, int((remaining+time.Millisecond-1)/time.Millisecond))
			if pollErr != unix.EINTR {
				break
			}
		}
		if ready <= 0 || pollErr != nil {
			framed = c.mark(framed, tea.KeyPressMsg{Code: tea.KeyEscape})
			c.sentEscape = true
		}
	}
	return len(framed), err
}

func (c *clipboardInput) frame(out, raw []byte) []byte {
	for _, b := range raw {
		if c.packet != nil {
			end := b == '\a' || (b == '\\' && c.lastEscape)
			c.lastEscape = b == '\x1b'
			if len(c.packet) < maxClipboardPacket {
				c.packet = append(c.packet, b)
			} else {
				c.oversized = true
			}
			if end {
				var msg tea.Msg = uv.UnknownOscEvent(string(c.packet))
				if c.oversized {
					msg = oversizedClipboardPacket{}
				}
				out = c.mark(out, msg)
				c.packet = nil
				c.oversized = false
				c.lastEscape = false
			}
			continue
		}
		// Bracketed paste is literal text, even if it quotes an OSC reply.
		delimiter := "\x1b[200~"
		if c.bracketed {
			delimiter = "\x1b[201~"
		}
		wasBracketed := c.bracketed
		if b == delimiter[c.pastePrefix] {
			c.pastePrefix++
			if c.pastePrefix == len(delimiter) {
				c.bracketed = !c.bracketed
				c.pastePrefix = 0
			}
		} else if b == '\x1b' {
			c.pastePrefix = 1
		} else {
			c.pastePrefix = 0
		}
		if wasBracketed {
			out = append(out, b)
			continue
		}
		if b == clipboardPrefix[c.prefix] {
			c.prefix++
			if c.prefix == len(clipboardPrefix) {
				c.packet = []byte(clipboardPrefix)
				c.prefix = 0
				c.sentEscape = false
			}
			continue
		}
		start := 0
		if c.sentEscape {
			start = 1
		}
		out = append(out, clipboardPrefix[start:c.prefix]...)
		c.prefix = 0
		c.sentEscape = false
		if b == '\x1b' {
			c.prefix = 1
		} else {
			out = append(out, b)
		}
	}

	return out
}

func (c *clipboardInput) restorePacket(_ tea.Model, msg tea.Msg) tea.Msg {
	event, ok := msg.(uv.UnknownOscEvent)
	if !ok || string(event) != clipboardPacketMarker {
		return msg
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.packets) == 0 {
		return nil
	}
	packet := c.packets[0]
	c.packets[0] = nil
	c.packets = c.packets[1:]
	return packet
}

func (c *clipboardInput) mark(out []byte, msg tea.Msg) []byte {
	c.mu.Lock()
	c.packets = append(c.packets, msg)
	c.mu.Unlock()
	return append(out, clipboardPacketMarker...)
}
