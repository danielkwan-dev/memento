//go:build embedfrontend

package main

import (
	"embed"
	"io/fs"

	"github.com/danielkwan-dev/memento/internal/api"
)

// The container build copies the Vite output here and compiles with the
// embedfrontend tag, so the image ships one binary that serves both the API and
// the app. Without the tag (ordinary local builds) this file is skipped entirely
// and no dist directory is required.
//
//go:embed all:dist
var distFS embed.FS

func init() {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("embed frontend: " + err.Error())
	}
	api.StaticFS = sub
}
