package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReadSSERejectsOversizeMultiLineEvent(t *testing.T) {
	// Each line is well under the per-line cap, but the event as a whole
	// (~5 MiB) exceeds the aggregate cap.
	line := "data: " + strings.Repeat("a", 512*1024) + "\n"
	body := strings.Repeat(line, 10) + "\n"
	ch := make(chan string, 4)
	_, err := readSSEState(context.Background(), strings.NewReader(body), ch)
	if !errors.Is(err, errSSEEventTooLarge) {
		t.Fatalf("err = %v, want errSSEEventTooLarge", err)
	}
	if len(ch) != 0 {
		t.Fatalf("oversize event was delivered (%d events)", len(ch))
	}
}

func TestReadSSEAcceptsEventAtAggregateLimit(t *testing.T) {
	// Four lines joined by three newlines land exactly on the aggregate cap.
	part := (maxSSEEventBytes - 3) / 4
	extra := (maxSSEEventBytes - 3) - 4*part
	var b strings.Builder
	for i := 0; i < 4; i++ {
		n := part
		if i == 0 {
			n += extra
		}
		b.WriteString("data: " + strings.Repeat("a", n) + "\n")
	}
	b.WriteString("\ndata: [DONE]\n\n")
	ch := make(chan string, 4)
	done, err := readSSEState(context.Background(), strings.NewReader(b.String()), ch)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v, want completed stream", done, err)
	}
	if got := len(<-ch); got != maxSSEEventBytes {
		t.Fatalf("event len = %d, want %d", got, maxSSEEventBytes)
	}
}

func TestStreamSSEOversizeEventSurfacesError(t *testing.T) {
	line := "data: " + strings.Repeat("a", 512*1024) + "\n"
	body := strings.Repeat(line, 10) + "\n"
	ch := streamSSE(context.Background(), strings.NewReader(body), func(data string) []StreamChunk {
		return []StreamChunk{{Content: "x"}}
	}, nil)
	var last StreamChunk
	for c := range ch {
		last = c
	}
	if !last.Done || last.Error == nil || !errors.Is(last.Error, errSSEEventTooLarge) {
		t.Fatalf("last chunk = %+v, want terminal oversize error", last)
	}
}

func TestStreamSSETerminalSendHonorsCancellationWithFullChannel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler SSEEventHandler
	}{
		{"reader terminal", func(string) []StreamChunk { return []StreamChunk{{Content: "x"}} }},
		{"handler terminal", func(data string) []StreamChunk {
			if data == "end" {
				return []StreamChunk{{Done: true}}
			}
			return []StreamChunk{{Content: "x"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 64 content events fill the output buffer; the stream then ends
			// (without [DONE] or with a handler Done chunk), so the goroutine
			// blocks on its terminal send while nobody reads.
			body := strings.Repeat("data: x\n\n", 64) + "data: end\n\n"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cleaned := make(chan struct{})
			ch := streamSSE(ctx, strings.NewReader(body), tc.handler, func() { close(cleaned) })

			deadline := time.Now().Add(2 * time.Second)
			for len(ch) < cap(ch) {
				if time.Now().After(deadline) {
					t.Fatalf("output channel never filled (len=%d)", len(ch))
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond) // let the goroutine reach the terminal send
			select {
			case <-cleaned:
				t.Fatal("goroutine exited before cancellation despite a full channel")
			default:
			}
			cancel()
			select {
			case <-cleaned:
			case <-time.After(2 * time.Second):
				t.Fatal("stream goroutine did not exit after cancellation while channel was full")
			}
		})
	}
}
