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
