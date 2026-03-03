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
