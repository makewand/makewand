package diag

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/makewand/makewand/internal/model"
)

// JSONLTraceSink writes router trace events to a JSONL file.
type JSONLTraceSink struct {
	mu sync.Mutex
	f  *os.File
}

// NewJSONLTraceSink opens path for append-only JSONL trace writes.
func NewJSONLTraceSink(path string) (*JSONLTraceSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &JSONLTraceSink{f: f}, nil
}

// OpenFirstJSONLTraceSink returns the first writable sink from the candidate paths.
func OpenFirstJSONLTraceSink(paths []string) (*JSONLTraceSink, string, error) {
	var lastErr error
	for _, path := range paths {
		sink, err := NewJSONLTraceSink(path)
		if err == nil {
			return sink, path, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no trace path candidates available")
	}
	return nil, "", lastErr
}

// Trace appends one trace event as a single JSONL line.
func (s *JSONLTraceSink) Trace(event model.TraceEvent) {
	b, err := json.Marshal(event)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.f.Write(b)
	_, _ = s.f.Write([]byte("\n"))
}

// Close closes the underlying trace file.
func (s *JSONLTraceSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}
