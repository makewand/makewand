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

func readSSEState(ctx context.Context, r io.Reader, ch chan<- string) (bool, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
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
			if dataBuilder.Len() > 0 {
				dataBuilder.WriteByte('\n')
			}
			dataBuilder.WriteString(strings.TrimPrefix(line, "data: "))
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
					if result.err != nil || !result.completed {
						if result.err == nil {
							result.err = fmt.Errorf("SSE stream ended before its completion marker")
						}
						ch <- StreamChunk{Done: true, Error: &execution.UnknownOutcomeError{Err: result.err}}
					} else {
						ch <- StreamChunk{Done: true}
					}
					return
				}
				for _, chunk := range handler(data) {
					if chunk.Done || chunk.Error != nil {
						ch <- chunk
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
