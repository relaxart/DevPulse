// Package migrations embeds the SQL migration files so the binary can apply the
// schema itself at startup, without shipping the .sql files separately.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
