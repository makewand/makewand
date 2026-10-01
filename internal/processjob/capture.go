package processjob

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// MaxOutputBytes is the combined raw stdout and stderr budget for a command.
// Count bytes before decoding, parsing or stripping terminal escape sequences.
const MaxOutputBytes = 10 << 20

// Capture owns one shared budget for both streams. On overflow it retains the
// available prefix, records discarded bytes and invokes stop exactly once.
// Returning the original write length lets exec drain and reap the child; a
// caller must reject Exceeded output rather than treating the prefix as success.
type Capture struct {
	mu        sync.Mutex
	streams   [2]bytes.Buffer
	dropped   [2]int64
	remaining int
	exceeded  bool
	stop      func()
}

func NewCapture(limit int, stop func()) *Capture {
	if limit < 0 {
		limit = 0
	}
	return &Capture{remaining: limit, stop: stop}
}

type captureWriter struct {
	capture *Capture
	stream  int
}

func (w captureWriter) Write(p []byte) (int, error) {
	c := w.capture
	c.mu.Lock()
	n := min(len(p), c.remaining)
	_, _ = c.streams[w.stream].Write(p[:n])
	c.remaining -= n
	c.dropped[w.stream] += int64(len(p) - n)
	firstOverflow := len(p) > n && !c.exceeded
	c.exceeded = c.exceeded || len(p) > n
	stop := c.stop
	c.mu.Unlock()
	if firstOverflow && stop != nil {
		stop()
	}
	return len(p), nil
}

func (c *Capture) Stdout() io.Writer { return captureWriter{capture: c, stream: 0} }
func (c *Capture) Stderr() io.Writer { return captureWriter{capture: c, stream: 1} }

func (c *Capture) StdoutString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[0].String()
}

func (c *Capture) StderrString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[1].String()
}

func (c *Capture) StdoutBytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.streams[0].Bytes())
}

func (c *Capture) Exceeded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exceeded
}

func (c *Capture) DroppedBytes() (stdout, stderr int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped[0], c.dropped[1]
}

func OutputLimitError() error {
	return fmt.Errorf("command exceeded the combined %d-byte raw stdout/stderr limit; execution outcome is unknown", MaxOutputBytes)
}
