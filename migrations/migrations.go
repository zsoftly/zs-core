// Package migrations embeds the forward-only SQL migrations of ZSoftly Core.
//
// Files are named NNNNNN_name.up.sql. They contain unqualified object names,
// so they apply to the first schema on the connection's search_path. There
// are no down migrations: a correction is a new migration.
package migrations

import "embed"

// FS holds every migration file.
//
//go:embed *.up.sql
var FS embed.FS
