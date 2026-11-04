// Package migrations embeds the identity schema migrations.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed *.sql
var files embed.FS

// FS returns the embedded .sql files for dbmigrate.Apply.
func FS() fs.FS {
	return files
}
