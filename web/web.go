// Package web holds the embedded web UI. Keeping the assets in the binary means the
// server is still a single file to deploy — no separate static directory to install or
// keep in sync.
package web

import (
	"embed"
	"io/fs"
)

// The logos are downscaled copies of img/arcatum-logo-2*.png: the originals are half a
// megabyte each, which is not a size to send a browser on every visit. The -dark ones
// are the same marks with the navy lifted to near-white — measured against the dark
// panel that navy sits at 1.65:1, which would sink half the shield and all of the
// wordmark into the background. They live here rather than being read from disk at
// runtime for the same reason as the rest: one file to deploy.
//
//go:embed index.html app.js style.css
//go:embed logo.png logo-dark.png logo-wordmark.png logo-wordmark-dark.png favicon.png
var assets embed.FS

// FS returns the embedded UI assets.
func FS() fs.FS { return assets }
