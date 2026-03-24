package serveradmin

import (
	"net/http"
	"strings"
)

func wantsCSV(req *http.Request) bool {
	if req == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.URL.Query().Get("format")), "csv")
}

func writeCSVHeaders(w http.ResponseWriter, filename string) {
	if w == nil {
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	if name := strings.TrimSpace(filename); name != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
}
