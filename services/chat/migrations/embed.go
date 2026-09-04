// Package migrations holds the goose migration files and exposes them as an
// embedded filesystem.
//
// Embedding rather than reading from disk means `cmd/migrate` is a single
// static binary that carries its own migrations, so the runtime image needs no
// copy of this directory and the test suite can build the schema without
// knowing where the repository root is. Migrations still run as an explicit,
// separate step — never from the server's startup path (CLAUDE.md).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
