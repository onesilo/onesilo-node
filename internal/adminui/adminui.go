// Package adminui embeds the node's localhost admin dashboard — a
// dependency-free static page (Silos / Models / Settings) served from the
// admin port. It follows the One Silo dashboard's design language but is
// hand-written HTML/CSS/JS so the node keeps its no-toolchain build: `go
// build` is still the whole story.
//
// The static assets are served without auth (the admin server binds
// 127.0.0.1 only); every API request the page makes carries the admin
// bearer token, which the page asks for once and keeps in localStorage.
package adminui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Handler serves the embedded UI at /.
func Handler() http.Handler {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		// Embed is compile-time; this cannot fail at runtime.
		panic(err)
	}
	return http.FileServerFS(sub)
}
