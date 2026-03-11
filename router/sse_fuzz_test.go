package router

import (
	"context"
	"strings"
	"testing"
)

func FuzzReadSSE(f *testing.F) {
	seeds := []string{
		"",
		"data: hello\n\n",
		"data: hello\ndata: world\n\n",
		"event: message\ndata: {\"ok\":true}\n\n",
		"data: [DONE]\n\n",
		"data: unterminated",
		strings.Repeat("a", 70_000),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, payload string) {
		ctx := context.Background()

		rawCh := make(chan string)
		rawDone := make(chan []string, 1)
		go func() {
			var events []string
			for data := range rawCh {
				events = append(events, data)
			}
			rawDone <- events
		}()

		err := readSSE(ctx, strings.NewReader(payload), rawCh)
		close(rawCh)
		events := <-rawDone
		if err == nil {
			for _, data := range events {
				if data == "" {
					t.Fatalf("readSSE(%q) emitted empty event", payload)
				}
				if data == "[DONE]" {
					t.Fatalf("readSSE(%q) forwarded [DONE]", payload)
				}
			}
		}

		streamCh := streamSSE(ctx, strings.NewReader(payload), func(data string) []StreamChunk {
			return []StreamChunk{{Content: data}}
		}, nil)
		for chunk := range streamCh {
			if chunk.Done || chunk.Error != nil {
				return
			}
		}
	})
}
