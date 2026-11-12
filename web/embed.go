// Package web holds the admin page, embedded into the server binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:admin
var files embed.FS

// Admin returns the admin page files, rooted at the directory that holds index.html.
func Admin() fs.FS {
	sub, err := fs.Sub(files, "admin")
	if err != nil {
		// The directory is embedded at build time, so this cannot happen at run time.
		panic(err)
	}
	return sub
}
