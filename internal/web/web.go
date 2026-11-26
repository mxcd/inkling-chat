// Package web serves the frontend embedded in the binary (DEP-001): the built files, and
// index.html for every other path, so a deep link opens the app. `bun run build` in ui/ writes
// html/dist; without it the committed placeholder html/index.html is served, so the embed
// compiles and the API works before the frontend was ever built.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:html
var files embed.FS

// assets is the directory of Vite's content-hashed files: a name there never changes content.
const assets = "assets/"

// Handler serves the embedded frontend: the build when there is one, else the placeholder.
func Handler() (http.Handler, error) {
	root, err := fs.Sub(files, "html")
	if err != nil {
		return nil, err
	}
	if built, err := fs.Sub(root, "dist"); err == nil && exists(built, "index.html") {
		root = built
	}
	return handler(root), nil
}

// handler serves root: a file as it is, a missing asset as 404 (a chunk of an older build must
// fail, not load index.html as script), anything else as index.html for the router.
func handler(root fs.FS) http.Handler {
	fileServer := http.FileServerFS(root)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+request.URL.Path), "/")
		switch {
		case strings.HasPrefix(name, assets) && exists(root, name):
			writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		case strings.HasPrefix(name, assets):
			http.NotFound(writer, request)
			return
		case name == "" || !exists(root, name):
			// index.html names the current build's assets: always revalidated after a deploy
			writer.Header().Set("Cache-Control", "no-cache")
			request = request.Clone(request.Context())
			request.URL.Path = "/"
		}
		fileServer.ServeHTTP(writer, request)
	})
}

// exists reports whether name is a file of root; a directory, the root included, is not.
func exists(root fs.FS, name string) bool {
	info, err := fs.Stat(root, name)
	return err == nil && !info.IsDir()
}
