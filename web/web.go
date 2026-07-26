// Package web holds the embedded web UI. Keeping the assets in the binary means the
// server is still a single file to deploy — no separate static directory to install or
// keep in sync.
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html app.js style.css
var assets embed.FS

// FS returns the embedded UI assets.
func FS() fs.FS { return assets }
