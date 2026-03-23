package serveraudit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one server-side audit record for an HTTP or session request.
type Event struct {
	Timestamp        time.Time `json:"timestamp"`
	Kind             string    `json:"kind,omitempty"`
	TokenID          string    `json:"token_id,omitempty"`
	TokenDescription string    `json:"token_description,omitempty"`
	Scope            string    `json:"scope,omitempty"`
	Method           string    `json:"method,omitempty"`
	Path             string    `json:"path,omitempty"`
	Status           int       `json:"status,omitempty"`
	DurationMS       int64     `json:"duration_ms,omitempty"`
	RequestedMode    string    `json:"requested_mode,omitempty"`
	RequestedModel   string    `json:"requested_model,omitempty"`
	ActualProvider   string    `json:"actual_provider,omitempty"`
	WorkspaceID      string    `json:"workspace_id,omitempty"`
	Error            string    `json:"error,omitempty"`
}

// Logger writes audit events.
type Logger interface {
	Log(Event)
}

// JSONLLogger appends audit events to a JSONL file.
type JSONLLogger struct {
	mu sync.Mutex
	f  *os.File
}

// OpenJSONL opens path for append-only JSONL audit writes.
func OpenJSONL(path string) (*JSONLLogger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &JSONLLogger{f: f}, nil
}

// Log appends evt as one JSONL record.
func (l *JSONLLogger) Log(evt Event) {
	if l == nil || l.f == nil {
		return
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = json.NewEncoder(l.f).Encode(evt)
}

// Close flushes and closes the underlying file.
func (l *JSONLLogger) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.f.Close()
	l.f = nil
	return err
}
