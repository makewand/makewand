package model

import (
	"bufio"
	"context"
	"io"
	"strings"
)

// sseEvent represents a parsed SSE event.
type sseEvent struct {
	Data string
}

// readSSE reads Server-Sent Events from r and sends raw data strings to ch.
// It exits when the reader is exhausted, ctx is cancelled, or "[DONE]" is received.
// The caller should close ch after this function returns.
func readSSE(ctx context.Context, r io.Reader, ch chan<- string) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var dataBuilder strings.Builder

	for scanner.Scan() {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line := scanner.Text()

		if line == "" {
			// Empty line = end of event
			if dataBuilder.Len() > 0 {
				data := dataBuilder.String()
				dataBuilder.Reset()

				if data == "[DONE]" {
					return nil
				}

				select {
				case ch <- data:
				case <-ctx.Done():
					return nil
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
		if data != "[DONE]" {
			select {
			case ch <- data:
			case <-ctx.Done():
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
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
		if cleanup != nil {
			defer cleanup()
		}

		dataCh := make(chan string, 64)
		errCh := make(chan error, 1)
		go func(dataCh chan<- string, errCh chan<- error) {
			defer close(dataCh)
			errCh <- readSSE(ctx, r, dataCh)
			close(errCh)
		}(dataCh, errCh)

		for {
			select {
			case <-ctx.Done():
				return
			case err, ok := <-errCh:
				if !ok {
					errCh = nil
					continue
				}
				errCh = nil
				if err != nil {
					ch <- StreamChunk{Error: err}
					return
				}
			case data, ok := <-dataCh:
				if !ok {
					ch <- StreamChunk{Done: true}
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
