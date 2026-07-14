// Package migrations embeds the SQL schema migrations so the binary stays
// self-contained — no separate migrations directory to ship.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
