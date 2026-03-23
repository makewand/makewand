package serverusage

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	Timestamp        time.Time `json:"timestamp"`
	RequestID        string    `json:"request_id,omitempty"`
	TokenID          string    `json:"token_id,omitempty"`
	TokenDescription string    `json:"token_description,omitempty"`
	RequestedMode    string    `json:"requested_mode,omitempty"`
	RequestedModel   string    `json:"requested_model,omitempty"`
	ActualProvider   string    `json:"actual_provider,omitempty"`
	Status           int       `json:"status,omitempty"`
	DurationMS       int64     `json:"duration_ms,omitempty"`
	PromptTokens     int       `json:"prompt_tokens,omitempty"`
	CompletionTokens int       `json:"completion_tokens,omitempty"`
	CostUSD          float64   `json:"cost_usd,omitempty"`
	Stream           bool      `json:"stream,omitempty"`
}

type Filter struct {
	TokenID    string
	Provider   string
	Status     int
	Since      time.Time
	Until      time.Time
	Limit      int
	StreamOnly bool
}

type Summary struct {
	TotalRequests         int                `json:"total_requests"`
	TotalPromptTokens     int                `json:"total_prompt_tokens,omitempty"`
	TotalCompletionTokens int                `json:"total_completion_tokens,omitempty"`
	TotalCostUSD          float64            `json:"total_cost_usd,omitempty"`
	ByToken               map[string]int     `json:"by_token,omitempty"`
	ByProvider            map[string]int     `json:"by_provider,omitempty"`
	CostByToken           map[string]float64 `json:"cost_by_token,omitempty"`
	CostByProvider        map[string]float64 `json:"cost_by_provider,omitempty"`
	Earliest              time.Time          `json:"earliest,omitempty"`
	Latest                time.Time          `json:"latest,omitempty"`
}

type Logger interface {
	Log(Entry)
}

type JSONLLogger struct {
	mu sync.Mutex
	f  *os.File
}

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

func (l *JSONLLogger) Log(entry Entry) {
	if l == nil || l.f == nil {
		return
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = json.NewEncoder(l.f).Encode(entry)
}

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

func LoadEntries(path string, filter Filter) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	entries := make([]Entry, 0, 64)
	dec := json.NewDecoder(bufio.NewReader(f))
	for {
		var entry Entry
		if err := dec.Decode(&entry); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if !matchesFilter(entry, filter) {
			continue
		}
		entries = append(entries, entry)
		if filter.Limit > 0 && len(entries) >= filter.Limit {
			break
		}
	}
	return entries, nil
}

func SummarizeEntries(entries []Entry) Summary {
	summary := Summary{
		ByToken:        make(map[string]int),
		ByProvider:     make(map[string]int),
		CostByToken:    make(map[string]float64),
		CostByProvider: make(map[string]float64),
	}
	for i, entry := range entries {
		summary.TotalRequests++
		summary.TotalPromptTokens += entry.PromptTokens
		summary.TotalCompletionTokens += entry.CompletionTokens
		summary.TotalCostUSD += entry.CostUSD
		if entry.TokenID != "" {
			summary.ByToken[entry.TokenID]++
			if entry.CostUSD > 0 {
				summary.CostByToken[entry.TokenID] += entry.CostUSD
			}
		}
		if entry.ActualProvider != "" {
			summary.ByProvider[entry.ActualProvider]++
			if entry.CostUSD > 0 {
				summary.CostByProvider[entry.ActualProvider] += entry.CostUSD
			}
		}
		if i == 0 || (!entry.Timestamp.IsZero() && entry.Timestamp.Before(summary.Earliest)) || summary.Earliest.IsZero() {
			summary.Earliest = entry.Timestamp
		}
		if entry.Timestamp.After(summary.Latest) {
			summary.Latest = entry.Timestamp
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

func matchesFilter(entry Entry, filter Filter) bool {
	if filter.TokenID != "" && entry.TokenID != filter.TokenID {
		return false
	}
	if filter.Provider != "" && !strings.EqualFold(entry.ActualProvider, filter.Provider) {
		return false
	}
	if filter.Status != 0 && entry.Status != filter.Status {
		return false
	}
	if filter.StreamOnly && !entry.Stream {
		return false
	}
	if !filter.Since.IsZero() && entry.Timestamp.Before(filter.Since) {
		return false
	}
	if !filter.Until.IsZero() && entry.Timestamp.After(filter.Until) {
		return false
	}
	return true
}
