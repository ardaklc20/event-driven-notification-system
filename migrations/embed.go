// Package migrations embeds the SQL migrations so cmd/migrate and the tests
// run the same files (K18).
package migrations

import (
	"database/sql"
	"embed"

	"github.com/pressly/goose/v3"
)

// FS holds every migration file.
//
//go:embed *.sql
var FS embed.FS

// NewProvider returns a goose Provider over db for the embedded migrations.
// It keeps no package-level state.
func NewProvider(db *sql.DB) (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectPostgres, db, FS)
}
