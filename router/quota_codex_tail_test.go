package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// GR-5: the Codex quota source reads the 8 newest session logs on every
// refresh, and a long session's log reaches hundreds of MB.

// writeLargeCodexLog writes a session log of at least size bytes: head, then
// lines that carry no quota data, then tail.
func writeLargeCodexLog(t *testing.T, path, head, tail string, size int) {
	t.Helper()
	var b strings.Builder
	b.WriteString(head)
	line, _ := json.Marshal(map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "type": "response_item",
		"payload": map[string]any{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": strings.Repeat("words ", 40)}}}})
	for b.Len() < size {
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteString(tail)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexSessionTailReadsAtMostTheLast4MiB_GR5(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "small.jsonl")
	if err := os.WriteFile(small, []byte("{\"a\":1}\n{\"b\":2}"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, _, err := readCodexSessionLines(small)
	if err != nil || string(data) != "{\"a\":1}\n{\"b\":2}" {
		t.Fatalf("small log = %q, %v; want it whole", data, err)
	}

	large := filepath.Join(dir, "large.jsonl")
	writeLargeCodexLog(t, large, "{\"first\":true}\n", "{\"last\":true}\n", 9<<20)
	raw, err := os.ReadFile(large)
	if err != nil {
		t.Fatal(err)
	}
	data, info, err := readCodexSessionLines(large)
	if err != nil || info == nil {
		t.Fatal(err)
	}
	// The log's first line, then its last complete lines: never the line the
	// tail starts inside, always through the end of the log.
	tail, found := bytes.CutPrefix(data, []byte("{\"first\":true}\n"))
	if !found {
		t.Fatalf("lines do not start with the log's first line")
	}
	if len(tail) == 0 || len(tail) > codexSessionTailBytes {
		t.Fatalf("read %d tail bytes of a %d-byte log, want at most %d", len(tail), len(raw), codexSessionTailBytes)
	}
	if !bytes.HasSuffix(raw, tail) || raw[len(raw)-len(tail)-1] != '\n' || !bytes.HasSuffix(tail, []byte("{\"last\":true}\n")) {
		t.Fatalf("tail is not the last complete lines of the log")
	}
}

// A large log is read only from its last 4 MiB, within a time bound, and the
// lines there that carry quota data still bind and count.
func TestCodexQuotaReadsOnlyTheTailOfLargeLogs_GR5(t *testing.T) {
	root := t.TempDir()
	writeQuotaTestAuth(t, root, "A")
	now := time.Now()
	// A newer observation of the same window, beyond the last 4 MiB and past
	// the log's first line, is not read.
	head := "{\"type\":\"turn_context\"}\n" + quotaTestEvent("A", 99, 300, now, "")
	meta := "{\"type\":\"session_meta\",\"payload\":{\"account_id\":\"A\"}}\n"
	unbound, _ := json.Marshal(map[string]any{"timestamp": now.Add(-time.Minute).Format(time.RFC3339Nano),
		"payload": map[string]any{"rate_limits": map[string]any{"secondary": map[string]any{
			"used_percent": 45, "window_minutes": 10080, "resets_at": now.Add(time.Hour).Unix()}}}})
	tail := quotaTestEvent("A", 30, 300, now.Add(-time.Minute), "") + meta + string(unbound) + "\n"
	sessions := filepath.Join(root, "sessions")
	writeLargeCodexLog(t, filepath.Join(sessions, "2026", "10", "21", "rollout-a.jsonl"), head, tail, 24<<20)
	writeLargeCodexLog(t, filepath.Join(sessions, "2026", "10", "21", "rollout-b.jsonl"), "", "", 24<<20)

	start := time.Now()
	q, err := NewCodexQuotaSource(sessions).Read(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if q.FiveHourPct == nil || *q.FiveHourPct != 30 {
		t.Fatalf("five-hour window = %v, want 30 from the log's tail (99 is beyond its last 4 MiB)", pctString(q.FiveHourPct))
	}
	if q.WeeklyPct == nil || *q.WeeklyPct != 45 {
		t.Fatalf("weekly window = %v, want 45 bound by the tail's session_meta", pctString(q.WeeklyPct))
	}
	t.Logf("read two 24 MiB logs in %s", elapsed)
	if elapsed > 3*time.Second {
		t.Fatalf("reading two 24 MiB logs took %s", elapsed)
	}
}

// Codex writes a session's session_meta as its log's first line. A session
// whose transcript runs past the 4 MiB tail still binds the tail's events that
// name no account to that line, which is read when complete within 64 KiB.
func TestCodexQuotaBindsTheTailToTheFirstLineSessionMeta(t *testing.T) {
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", t.TempDir())
	now := time.Now()
	unbound, _ := json.Marshal(map[string]any{"timestamp": now.Add(-time.Minute).Format(time.RFC3339Nano),
		"payload": map[string]any{"rate_limits": map[string]any{"primary": map[string]any{
			"used_percent": 45, "window_minutes": 10080, "resets_at": now.Add(time.Hour).Unix()}}}})
	sessionMeta := func(account string, instructions int) string {
		raw, _ := json.Marshal(map[string]any{"timestamp": now.Add(-time.Hour).Format(time.RFC3339Nano), "type": "session_meta",
			"payload": map[string]any{"account_id": account, "base_instructions": map[string]string{"text": strings.Repeat("i", instructions)}}})
		return string(raw) + "\n"
	}
	for _, tc := range []struct {
		name   string
		first  string
		weekly float64 // 0: no current evidence
	}{
		// Codex's own session_meta lines are 20 to 23 KB.
		{"this account's session_meta", sessionMeta("A", 22<<10), 45},
		{"another account's session_meta", sessionMeta("B", 22<<10), 0},
		{"a first line past 64 KiB", sessionMeta("A", 64<<10), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeQuotaTestAuth(t, root, "A")
			sessions := filepath.Join(root, "sessions")
			// More than 4 MiB of transcript follows the first line.
			writeLargeCodexLog(t, filepath.Join(sessions, "rollout.jsonl"), tc.first, string(unbound)+"\n", len(tc.first)+codexSessionTailBytes+(1<<20))
			q, err := NewCodexQuotaSource(sessions).Read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.weekly == 0 && (q.HasData || q.WeeklyPct != nil):
				t.Fatalf("weekly window = %s, want no evidence", pctString(q.WeeklyPct))
			case tc.weekly != 0 && (q.WeeklyPct == nil || *q.WeeklyPct != tc.weekly):
				t.Fatalf("weekly window = %s, want %v bound by the first line's session_meta", pctString(q.WeeklyPct), tc.weekly)
			}
		})
	}
}

func pctString(p *float64) string {
	if p == nil {
		return "<nil>"
	}
	return strconv.FormatFloat(*p, 'g', -1, 64)
}

// errAfterContext reports cancellation from its n-th Err call on.
type errAfterContext struct {
	context.Context
	calls atomic.Int64
	n     int64
}

func (c *errAfterContext) Err() error {
	if c.calls.Add(1) >= c.n {
		return context.Canceled
	}
	return nil
}

// A deadline that passes while one log is being scanned ends the read there.
func TestCodexQuotaChecksContextWithinALog_GR5(t *testing.T) {
	root := t.TempDir()
	writeQuotaTestAuth(t, root, "A")
	sessions := filepath.Join(root, "sessions")
	writeLargeCodexLog(t, filepath.Join(sessions, "rollout.jsonl"), "", quotaTestEvent("A", 30, 300, time.Now(), ""), 1<<20)
	ctx := &errAfterContext{Context: context.Background(), n: 3}
	if _, err := NewCodexQuotaSource(sessions).Read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read = %v, want the cancellation seen within the log", err)
	}
}
