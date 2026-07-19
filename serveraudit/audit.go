package serveraudit

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one server-side audit record for an HTTP or session request.
type Event struct {
	Timestamp        time.Time `json:"timestamp"`
	RequestID        string    `json:"request_id,omitempty"`
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
	PromptTokens     int       `json:"prompt_tokens,omitempty"`
	CompletionTokens int       `json:"completion_tokens,omitempty"`
	CostUSD          float64   `json:"cost_usd,omitempty"`
	Error            string    `json:"error,omitempty"`
}

// Filter narrows audit events when reading from JSONL.
type Filter struct {
	TokenID     string
	Kind        string
	WorkspaceID string
	Since       time.Time
	Until       time.Time
	Status      int
	Limit       int
}

// Summary aggregates audit activity for operator review.
type Summary struct {
	Total                 int                `json:"total"`
	ByKind                map[string]int     `json:"by_kind,omitempty"`
	ByStatus              map[int]int        `json:"by_status,omitempty"`
	ByToken               map[string]int     `json:"by_token,omitempty"`
	ByProvider            map[string]int     `json:"by_provider,omitempty"`
	TotalPromptTokens     int                `json:"total_prompt_tokens,omitempty"`
	TotalCompletionTokens int                `json:"total_completion_tokens,omitempty"`
	TotalCostUSD          float64            `json:"total_cost_usd,omitempty"`
	CostByToken           map[string]float64 `json:"cost_by_token,omitempty"`
	CostByProvider        map[string]float64 `json:"cost_by_provider,omitempty"`
	Earliest              time.Time          `json:"earliest,omitempty"`
	Latest                time.Time          `json:"latest,omitempty"`
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

// LoadEvents reads and filters audit events from a JSONL file.
func LoadEvents(path string, filter Filter) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	events := make([]Event, 0, 64)
	dec := json.NewDecoder(bufio.NewReader(f))
	for {
		var evt Event
		if err := dec.Decode(&evt); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if !matchesFilter(evt, filter) {
			continue
		}
		events = append(events, evt)
		if filter.Limit > 0 && len(events) >= filter.Limit {
			break
		}
	}
	return events, nil
}

// SummarizeEvents folds events into operator-friendly counters.
func SummarizeEvents(events []Event) Summary {
	summary := Summary{
		ByKind:         make(map[string]int),
		ByStatus:       make(map[int]int),
		ByToken:        make(map[string]int),
		ByProvider:     make(map[string]int),
		CostByToken:    make(map[string]float64),
		CostByProvider: make(map[string]float64),
	}
	for i, evt := range events {
		summary.Total++
		if evt.Kind != "" {
			summary.ByKind[evt.Kind]++
		}
		if evt.Status != 0 {
			summary.ByStatus[evt.Status]++
		}
		if evt.TokenID != "" {
			summary.ByToken[evt.TokenID]++
		}
		if evt.ActualProvider != "" {
			summary.ByProvider[evt.ActualProvider]++
		}
		summary.TotalPromptTokens += evt.PromptTokens
		summary.TotalCompletionTokens += evt.CompletionTokens
		summary.TotalCostUSD += evt.CostUSD
		if evt.TokenID != "" && evt.CostUSD > 0 {
			summary.CostByToken[evt.TokenID] += evt.CostUSD
		}
		if evt.ActualProvider != "" && evt.CostUSD > 0 {
			summary.CostByProvider[evt.ActualProvider] += evt.CostUSD
		}
		if i == 0 || (!evt.Timestamp.IsZero() && evt.Timestamp.Before(summary.Earliest)) || summary.Earliest.IsZero() {
			summary.Earliest = evt.Timestamp
		}
		if evt.Timestamp.After(summary.Latest) {
			summary.Latest = evt.Timestamp
		}
	}
	return summary
}

func SortedStringCounts(m map[string]int) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func SortedStatusCounts(m map[int]int) []int {
	if len(m) == 0 {
		return nil
	}
	keys := make([]int, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

func SortedStringTotals(m map[string]float64) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// WriteEventsCSV renders audit events as CSV.
func WriteEventsCSV(w io.Writer, events []Event) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"timestamp",
		"request_id",
		"kind",
		"token_id",
		"token_description",
		"scope",
		"method",
		"path",
		"status",
		"duration_ms",
		"requested_mode",
		"requested_model",
		"actual_provider",
		"workspace_id",
		"prompt_tokens",
		"completion_tokens",
		"cost_usd",
		"error",
	}); err != nil {
		return err
	}
	for _, evt := range events {
		if err := cw.Write([]string{
			evt.Timestamp.UTC().Format(time.RFC3339Nano),
			evt.RequestID,
			evt.Kind,
			evt.TokenID,
			evt.TokenDescription,
			evt.Scope,
			evt.Method,
			evt.Path,
			strconv.Itoa(evt.Status),
			strconv.FormatInt(evt.DurationMS, 10),
			evt.RequestedMode,
			evt.RequestedModel,
			evt.ActualProvider,
			evt.WorkspaceID,
			strconv.Itoa(evt.PromptTokens),
			strconv.Itoa(evt.CompletionTokens),
			strconv.FormatFloat(evt.CostUSD, 'f', 6, 64),
			evt.Error,
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func matchesFilter(evt Event, filter Filter) bool {
	if filter.TokenID != "" && evt.TokenID != filter.TokenID {
		return false
	}
	if filter.Kind != "" && !strings.EqualFold(evt.Kind, filter.Kind) {
		return false
	}
	if filter.WorkspaceID != "" && evt.WorkspaceID != filter.WorkspaceID {
		return false
	}
	if filter.Status != 0 && evt.Status != filter.Status {
		return false
	}
	if !filter.Since.IsZero() && evt.Timestamp.Before(filter.Since) {
		return false
	}
	if !filter.Until.IsZero() && evt.Timestamp.After(filter.Until) {
		return false
	}
	return true
}
