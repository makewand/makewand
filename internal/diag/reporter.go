package diag

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Reporter emits human-readable diagnostics while retaining a structured slog API.
type Reporter struct {
	logger *slog.Logger
}

// Stderr returns a reporter bound to the current process stderr.
func Stderr() *Reporter {
	return New(os.Stderr)
}

// New returns a reporter that writes diagnostics to w.
func New(w io.Writer) *Reporter {
	return &Reporter{
		logger: slog.New(&humanHandler{w: w}),
	}
}

// ErrorErr writes an error with an attached error value.
func (r *Reporter) ErrorErr(msg string, err error) {
	if err == nil {
		r.logger.LogAttrs(context.Background(), slog.LevelError, msg)
		return
	}
	r.logger.LogAttrs(context.Background(), slog.LevelError, msg, slog.Any("error", err))
}

// ErrorText writes an error without structured attributes.
func (r *Reporter) ErrorText(msg string) {
	r.logger.LogAttrs(context.Background(), slog.LevelError, msg)
}

// WarnErr writes a warning with an attached error value.
func (r *Reporter) WarnErr(msg string, err error) {
	if err == nil {
		r.logger.LogAttrs(context.Background(), slog.LevelWarn, msg)
		return
	}
	r.logger.LogAttrs(context.Background(), slog.LevelWarn, msg, slog.Any("error", err))
}

// InfoPath writes an informational message with a filesystem path.
func (r *Reporter) InfoPath(msg, path string) {
	r.logger.LogAttrs(context.Background(), slog.LevelInfo, msg, slog.String("path", path))
}

type humanHandler struct {
	w      io.Writer
	mu     sync.Mutex
	attrs  []slog.Attr
	groups []string
}

func (h *humanHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *humanHandler) Handle(_ context.Context, rec slog.Record) error {
	attrs := append([]slog.Attr(nil), h.attrs...)
	rec.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})

	line := formatRecord(rec, attrs, h.groups)
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, line+"\n")
	return err
}

func (h *humanHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &humanHandler{
		w:      h.w,
		attrs:  append(append([]slog.Attr(nil), h.attrs...), attrs...),
		groups: append([]string(nil), h.groups...),
	}
}

func (h *humanHandler) WithGroup(name string) slog.Handler {
	return &humanHandler{
		w:      h.w,
		attrs:  append([]slog.Attr(nil), h.attrs...),
		groups: append(append([]string(nil), h.groups...), name),
	}
}

func formatRecord(rec slog.Record, attrs []slog.Attr, groups []string) string {
	line := rec.Message
	if rec.Level >= slog.LevelError {
		line = "Error: " + line
	} else if rec.Level >= slog.LevelWarn {
		line = "Warning: " + line
	}

	var (
		errText  string
		pathText string
		extras   []string
	)

	for _, attr := range attrs {
		key, value, ok := flattenAttr(attr, groups)
		if !ok {
			continue
		}
		switch key {
		case "error":
			errText = value
		case "path":
			pathText = value
		default:
			extras = append(extras, key+"="+value)
		}
	}

	if errText != "" {
		line += ": " + errText
	}
	if pathText != "" {
		line += ": " + pathText
	}
	if len(extras) > 0 {
		line += " " + strings.Join(extras, " ")
	}
	return line
}

func flattenAttr(attr slog.Attr, groups []string) (string, string, bool) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return "", "", false
	}

	key := attr.Key
	if key == "" {
		return "", "", false
	}
	if len(groups) > 0 {
		key = strings.Join(append(append([]string(nil), groups...), key), ".")
	}

	if attr.Value.Kind() == slog.KindGroup {
		var flattened []string
		for _, child := range attr.Value.Group() {
			childKey, childValue, ok := flattenAttr(child, append(groups, attr.Key))
			if !ok {
				continue
			}
			flattened = append(flattened, childKey+"="+childValue)
		}
		return key, strings.Join(flattened, " "), len(flattened) > 0
	}

	return key, formatValue(attr.Value), true
}

func formatValue(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		return value.String()
	case slog.KindInt64:
		return strconv.FormatInt(value.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(value.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(value.Float64(), 'f', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(value.Bool())
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindTime:
		return value.Time().Format("2006-01-02T15:04:05Z07:00")
	case slog.KindAny:
		return fmt.Sprint(value.Any())
	default:
		return value.String()
	}
}
