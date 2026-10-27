// Package webui embeds the built web app (web/ -> internal/webui/dist).
// The Docker build replaces dist/ with the real build; the committed
// placeholder keeps plain `go build` working.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var files embed.FS

// FS returns the web app files.
func FS() fs.FS {
	sub, err := fs.Sub(files, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
