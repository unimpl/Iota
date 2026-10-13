// Package web embeds the React client so iota-server is a standalone executable.
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist/*
var assets embed.FS

func Files() fs.FS {
	files, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	return files
}
