// quota_helpers.go — small stdlib helpers for the quota sources.
package router

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

var errNoToken = errors.New("no oauth access token in credentials")

// httpGetJSON performs a GET with the given headers and returns the body bytes,
// failing on non-2xx status.
func httpGetJSON(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httpStatusError{
			Status:  resp.StatusCode,
			Message: "usage endpoint HTTP " + strconv.Itoa(resp.StatusCode),
		}
	}
	return body, nil
}

// recentJSONL returns up to `limit` .jsonl files under dir (recursively), newest
// mtime first.
func recentJSONL(dir string, limit int) ([]string, error) {
	type entry struct {
		path string
		mod  time.Time
	}
	var entries []entry
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees
		}
		if d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		entries = append(entries, entry{path: path, mod: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.After(entries[j].mod) })
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.path
	}
	return out, nil
}

// parseTime parses an RFC3339 timestamp, returning the zero time on failure.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}

// parseUnixOrTime accepts either a JSON number (unix seconds) or an RFC3339
// string and returns the corresponding time.
func parseUnixOrTime(v any) time.Time {
	switch x := v.(type) {
	case float64:
		return time.Unix(int64(x), 0)
	case string:
		if secs, err := strconv.ParseInt(x, 10, 64); err == nil {
			return time.Unix(secs, 0)
		}
		return parseTime(x)
	}
	return time.Time{}
}
