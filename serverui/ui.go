package serverui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static/*
var staticFS embed.FS

func Handler() http.Handler {
	assets, _ := fs.Sub(staticFS, "static")
	fileServer := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		if path == "/admin" || path == "/admin/" {
			http.ServeFileFS(w, req, assets, "index.html")
			return
		}
		if strings.HasPrefix(path, "/admin/") {
			trimmed := strings.TrimPrefix(path, "/admin/")
			if trimmed == "" || !strings.Contains(trimmed, ".") {
				http.ServeFileFS(w, req, assets, "index.html")
				return
			}
			req.URL.Path = "/" + trimmed
			fileServer.ServeHTTP(w, req)
			return
		}
		http.NotFound(w, req)
	})
}
