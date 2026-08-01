// Package webui embeds the management dashboard (plain HTML/CSS/JS) into
// the binary. Note: go:embed paths must live inside this package directory.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web
var webFiles embed.FS

// Handler serves the embedded dashboard files.
func Handler() http.Handler {
	sub, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err) // embedded tree is fixed at compile time
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(sub)
	routes := map[string]bool{
		"/setup/pair":         true,
		"/setup/printers":     true,
		"/setup/pos":          true,
		"/operations/jobs":    true,
		"/operations/queue":   true,
		"/system/diagnostics": true,
		"/system/logs":        true,
		"/dev/pos-simulator":  true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "" {
			http.Redirect(w, r, "/admin/operations/jobs", http.StatusFound)
			return
		}
		if routes[r.URL.Path] {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(index)
			}
			return
		}
		files.ServeHTTP(w, r)
	})
}
