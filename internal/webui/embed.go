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
	return http.FileServerFS(sub)
}
