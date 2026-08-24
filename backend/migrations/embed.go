// Package migrations embeds the SQL migration files so the binary can apply
// them without the .sql files being present on disk at runtime.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
