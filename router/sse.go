package router

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/makewand/makewand/execution"
)

// readSSE reads Server-Sent Events from r and sends raw data strings to ch.
// It exits when the reader is exhausted, ctx is cancelled, or "[DONE]" is received.
// The caller should close ch after this function returns.
func readSSE(ctx context.Context, r io.Reader, ch chan<- string) error {
	_, err := readSSEState(ctx, r, ch)
	return err
}

// maxSSELineBytes bounds a single SSE line; maxSSEEventBytes bounds the
// aggregate data payload of one event, which may span many "data:" lines.
// Without the aggregate bound a peer could stream an arbitrarily large event
// made of individually-small lines and exhaust memory.
const (
	maxSSELineBytes  = 2 * 1024 * 1024
	maxSSEEventBytes = 4 * 1024 * 1024
)

// errSSEEventTooLarge reports an SSE event whose aggregate data exceeds maxSSEEventBytes.
var errSSEEventTooLarge = fmt.Errorf("SSE event exceeds %d bytes", maxSSEEventBytes)

func readSSEState(ctx context.Context, r io.Reader, ch chan<- string) (bool, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxSSELineBytes)
	var dataBuilder strings.Builder

	for scanner.Scan() {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return false, nil
		default:
		}

		line := scanner.Text()

		if line == "" {
			// Empty line = end of event
			if dataBuilder.Len() > 0 {
				data := dataBuilder.String()
				dataBuilder.Reset()

				if data == "[DONE]" {
					return true, nil
				}

				select {
				case ch <- data:
				case <-ctx.Done():
					return false, nil
				}
			}
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			sep := 0
			if dataBuilder.Len() > 0 {
				sep = 1
			}
			if dataBuilder.Len()+sep+len(payload) > maxSSEEventBytes {
				return false, errSSEEventTooLarge
			}
			if sep == 1 {
				dataBuilder.WriteByte('\n')
			}
			dataBuilder.WriteString(payload)
		}
	}

	// Flush any remaining data
	if dataBuilder.Len() > 0 {
		data := dataBuilder.String()
		if data == "[DONE]" {
			return true, nil
		}
		if data != "[DONE]" {
			select {
			case ch <- data:
			case <-ctx.Done():
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// SSEEventHandler converts a raw SSE data string into zero or more StreamChunks.
// Return nil to skip an event, or a chunk with Done=true to signal completion.
type SSEEventHandler func(data string) []StreamChunk

// streamSSE manages the common SSE → StreamChunk channel pipeline used by
// Claude, OpenAI, and Gemini. It reads SSE events, calls handler for each,
// and forwards the resulting chunks. cleanup (if non-nil) is called when the
// stream goroutine exits (e.g. to close the response body).
func streamSSE(ctx context.Context, r io.Reader, handler SSEEventHandler, cleanup func()) <-chan StreamChunk {
	ch := make(chan StreamChunk, 64)
	go func() {
		defer close(ch)
		readCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if cleanup != nil {
			defer cleanup()
		}

		dataCh := make(chan string, 64)
		type readResult struct {
			completed bool
			err       error
		}
		resultCh := make(chan readResult, 1)
		go func() {
			defer close(dataCh)
			completed, err := readSSEState(readCtx, r, dataCh)
			resultCh <- readResult{completed: completed, err: err}
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case data, ok := <-dataCh:
				if !ok {
					result := <-resultCh
					terminal := StreamChunk{Done: true}
					if result.err != nil || !result.completed {
						if result.err == nil {
							result.err = fmt.Errorf("SSE stream ended before its completion marker")
						}
						terminal.Error = &execution.UnknownOutcomeError{Err: result.err}
					}
					// A consumer that stopped reading must not pin this
					// goroutine (and the response body) after cancellation.
					select {
					case ch <- terminal:
					case <-ctx.Done():
					}
					return
				}
				for _, chunk := range handler(data) {
					if chunk.Done || chunk.Error != nil {
						select {
						case ch <- chunk:
						case <-ctx.Done():
						}
						return
					}
					if chunk.Content != "" {
						select {
						case ch <- chunk:
						case <-ctx.Done():
							return
						}
					}
				}
			}
		}
	}()
	return ch
}
