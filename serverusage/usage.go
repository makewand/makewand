package serverusage

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

type Entry struct {
	Timestamp        time.Time `json:"timestamp"`
	RequestID        string    `json:"request_id,omitempty"`
	TokenID          string    `json:"token_id,omitempty"`
	TokenDescription string    `json:"token_description,omitempty"`
	UserID           string    `json:"user_id,omitempty"`
	OrganizationID   string    `json:"organization_id,omitempty"`
	ProjectID        string    `json:"project_id,omitempty"`
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
	RequestID  string
	TokenID    string
	UserID     string
	OrgID      string
	ProjectID  string
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
	ByUser                map[string]int     `json:"by_user,omitempty"`
	ByOrganization        map[string]int     `json:"by_organization,omitempty"`
	ByProject             map[string]int     `json:"by_project,omitempty"`
	ByProvider            map[string]int     `json:"by_provider,omitempty"`
	CostByToken           map[string]float64 `json:"cost_by_token,omitempty"`
	CostByUser            map[string]float64 `json:"cost_by_user,omitempty"`
	CostByOrganization    map[string]float64 `json:"cost_by_organization,omitempty"`
	CostByProject         map[string]float64 `json:"cost_by_project,omitempty"`
	CostByProvider        map[string]float64 `json:"cost_by_provider,omitempty"`
	Earliest              time.Time          `json:"earliest,omitempty"`
	Latest                time.Time          `json:"latest,omitempty"`
}

type PeriodSummary struct {
	Period                string  `json:"period"`
	TotalRequests         int     `json:"total_requests"`
	TotalPromptTokens     int     `json:"total_prompt_tokens,omitempty"`
	TotalCompletionTokens int     `json:"total_completion_tokens,omitempty"`
	TotalCostUSD          float64 `json:"total_cost_usd,omitempty"`
}

type Logger interface {
	Log(Entry)
}

// Reader loads usage entries from a backing store.
type Reader interface {
	Load(Filter) ([]Entry, error)
}

type JSONLLogger struct {
	mu sync.Mutex
	f  *os.File
}

// JSONLReader loads usage entries from a JSONL file path.
type JSONLReader struct {
	path string
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

// NewJSONLReader returns a Reader backed by a JSONL file path.
func NewJSONLReader(path string) *JSONLReader {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	return &JSONLReader{path: path}
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

// Load reads entries from the configured JSONL path.
func (r *JSONLReader) Load(filter Filter) ([]Entry, error) {
	if r == nil || strings.TrimSpace(r.path) == "" {
		return nil, nil
	}
	entries, err := LoadEntries(r.path, filter)
	if err != nil && os.IsNotExist(err) {
		return nil, nil
	}
	return entries, err
}

func SummarizeEntries(entries []Entry) Summary {
	summary := Summary{
		ByToken:            make(map[string]int),
		ByUser:             make(map[string]int),
		ByOrganization:     make(map[string]int),
		ByProject:          make(map[string]int),
		ByProvider:         make(map[string]int),
		CostByToken:        make(map[string]float64),
		CostByUser:         make(map[string]float64),
		CostByOrganization: make(map[string]float64),
		CostByProject:      make(map[string]float64),
		CostByProvider:     make(map[string]float64),
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
		if entry.UserID != "" {
			summary.ByUser[entry.UserID]++
			if entry.CostUSD > 0 {
				summary.CostByUser[entry.UserID] += entry.CostUSD
			}
		}
		if entry.OrganizationID != "" {
			summary.ByOrganization[entry.OrganizationID]++
			if entry.CostUSD > 0 {
				summary.CostByOrganization[entry.OrganizationID] += entry.CostUSD
			}
		}
		if entry.ProjectID != "" {
			summary.ByProject[entry.ProjectID]++
			if entry.CostUSD > 0 {
				summary.CostByProject[entry.ProjectID] += entry.CostUSD
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

// MonthStart returns the first instant of the month in UTC for the provided time.
func MonthStart(at time.Time) time.Time {
	at = at.UTC()
	return time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// CurrentMonthFilter returns a copy of filter constrained to the current month
// when no explicit since/until window was already provided.
func CurrentMonthFilter(filter Filter, now time.Time) Filter {
	if !filter.Since.IsZero() || !filter.Until.IsZero() {
		return filter
	}
	filter.Since = MonthStart(now)
	return filter
}

// WriteEntriesCSV renders usage entries as CSV.
func WriteEntriesCSV(w io.Writer, entries []Entry) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"timestamp",
		"request_id",
		"token_id",
		"token_description",
		"user_id",
		"organization_id",
		"project_id",
		"requested_mode",
		"requested_model",
		"actual_provider",
		"status",
		"duration_ms",
		"prompt_tokens",
		"completion_tokens",
		"cost_usd",
		"stream",
	}); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := cw.Write([]string{
			entry.Timestamp.UTC().Format(time.RFC3339Nano),
			entry.RequestID,
			entry.TokenID,
			entry.TokenDescription,
			entry.UserID,
			entry.OrganizationID,
			entry.ProjectID,
			entry.RequestedMode,
			entry.RequestedModel,
			entry.ActualProvider,
			strconv.Itoa(entry.Status),
			strconv.FormatInt(entry.DurationMS, 10),
			strconv.Itoa(entry.PromptTokens),
			strconv.Itoa(entry.CompletionTokens),
			strconv.FormatFloat(entry.CostUSD, 'f', 6, 64),
			strconv.FormatBool(entry.Stream),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WritePeriodsCSV renders billing period summaries as CSV.
func WritePeriodsCSV(w io.Writer, periods []PeriodSummary) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"period",
		"total_requests",
		"total_prompt_tokens",
		"total_completion_tokens",
		"total_cost_usd",
	}); err != nil {
		return err
	}
	for _, period := range periods {
		if err := cw.Write([]string{
			period.Period,
			strconv.Itoa(period.TotalRequests),
			strconv.Itoa(period.TotalPromptTokens),
			strconv.Itoa(period.TotalCompletionTokens),
			strconv.FormatFloat(period.TotalCostUSD, 'f', 6, 64),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func SummarizeMonthlyPeriods(entries []Entry) []PeriodSummary {
	if len(entries) == 0 {
		return nil
	}
	buckets := make(map[string]*PeriodSummary)
	for _, entry := range entries {
		period := entry.Timestamp.UTC().Format("2006-01")
		bucket, ok := buckets[period]
		if !ok {
			bucket = &PeriodSummary{Period: period}
			buckets[period] = bucket
		}
		bucket.TotalRequests++
		bucket.TotalPromptTokens += entry.PromptTokens
		bucket.TotalCompletionTokens += entry.CompletionTokens
		bucket.TotalCostUSD += entry.CostUSD
	}
	periods := make([]string, 0, len(buckets))
	for period := range buckets {
		periods = append(periods, period)
	}
	sort.Strings(periods)
	out := make([]PeriodSummary, 0, len(periods))
	for _, period := range periods {
		out = append(out, *buckets[period])
	}
	return out
}

func matchesFilter(entry Entry, filter Filter) bool {
	if filter.TokenID != "" && entry.TokenID != filter.TokenID {
		return false
	}
	if filter.UserID != "" && entry.UserID != filter.UserID {
		return false
	}
	if filter.OrgID != "" && entry.OrganizationID != filter.OrgID {
		return false
	}
	if filter.ProjectID != "" && entry.ProjectID != filter.ProjectID {
		return false
	}
	if filter.RequestID != "" && entry.RequestID != filter.RequestID {
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
