package model

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadSSE_LargeEvent(t *testing.T) {
	payload := "data: " + strings.Repeat("a", 100*1024) + "\n\n"
	ch := make(chan string, 1)

	err := readSSE(context.Background(), strings.NewReader(payload), ch)
	if err != nil {
		t.Fatalf("readSSE() error: %v", err)
	}
	close(ch)

	got, ok := <-ch
	if !ok {
		t.Fatal("expected one SSE event, got none")
	}
	if len(got) != 100*1024 {
		t.Fatalf("event size = %d, want %d", len(got), 100*1024)
	}
}

func TestReadSSE_PropagatesScannerError(t *testing.T) {
	wantErr := errors.New("boom")
	ch := make(chan string, 1)

	err := readSSE(context.Background(), errReader{err: wantErr}, ch)
	if !errors.Is(err, wantErr) {
		t.Fatalf("readSSE() error = %v, want %v", err, wantErr)
	}
}

type errReader struct {
	err error
}

func (r errReader) Read(p []byte) (int, error) {
	return 0, r.err
}

var _ io.Reader = errReader{}

func TestStreamSSE_RoutesEventsViaHandler(t *testing.T) {
	payload := "data: {\"text\":\"hello\"}\n\ndata: {\"text\":\"world\"}\n\ndata: [DONE]\n\n"

	ch := streamSSE(context.Background(), strings.NewReader(payload), func(data string) []StreamChunk {
		if data == "[DONE]" {
			return []StreamChunk{{Done: true}}
		}
		return []StreamChunk{{Content: data}}
	}, nil)

	var got []StreamChunk
	for chunk := range ch {
		got = append(got, chunk)
	}

	if len(got) < 2 {
		t.Fatalf("got %d chunks, want at least 2", len(got))
	}
	if got[0].Content != `{"text":"hello"}` {
		t.Fatalf("chunk[0].Content = %q, want hello event", got[0].Content)
	}
}

func TestStreamSSE_CallsCleanup(t *testing.T) {
	payload := "data: hi\n\n"
	cleaned := false

	ch := streamSSE(context.Background(), strings.NewReader(payload), func(data string) []StreamChunk {
		return []StreamChunk{{Content: data}}
	}, func() { cleaned = true })

	for range ch {
	}

	if !cleaned {
		t.Fatal("cleanup function was not called")
	}
}

func TestStreamSSE_HandlerDoneStopsStream(t *testing.T) {
	payload := "data: first\n\ndata: second\n\ndata: third\n\n"

	ch := streamSSE(context.Background(), strings.NewReader(payload), func(data string) []StreamChunk {
		if data == "second" {
			return []StreamChunk{{Done: true}}
		}
		return []StreamChunk{{Content: data}}
	}, nil)

	var got []StreamChunk
	for chunk := range ch {
		got = append(got, chunk)
	}

	// Should have "first" content + "second" done, not "third"
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2 (content + done)", len(got))
	}
	if got[0].Content != "first" {
		t.Fatalf("chunk[0].Content = %q, want first", got[0].Content)
	}
	if !got[1].Done {
		t.Fatal("chunk[1].Done = false, want true")
	}
}
