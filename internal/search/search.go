// Package search provides FTS5-backed queries with a LIKE fallback.
package search

import (
	"context"
	"database/sql"
	"strings"

	"gofind/internal/db"
)

// File is a single search result.
type File = db.File

const ftsSQL = `SELECT f.path, f.name, f.size, f.ext, f.mtime
FROM files_fts JOIN files f ON f.id = files_fts.rowid
WHERE files_fts MATCH ?
ORDER BY bm25(files_fts, 10.0, 1.0), f.size DESC
LIMIT ?`

const likeSQL = `SELECT f.path, f.name, f.size, f.ext, f.mtime
FROM files f
WHERE f.name LIKE ? ESCAPE '\' OR f.path LIKE ? ESCAPE '\'
ORDER BY f.size DESC
LIMIT ?`

// tokens splits a query on whitespace.
func tokens(query string) []string {
	return strings.Fields(query)
}

// ftsExpr builds a safe FTS5 MATCH expression: each whitespace token is
// double-quoted (embedded quotes doubled) and made a prefix match, joined by
// spaces (AND semantics). Tokens may contain quotes, *, -, (, :, etc.
func ftsExpr(query string) string {
	var sb strings.Builder
	for i, tok := range tokens(query) {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteByte('"')
		sb.WriteString(strings.ReplaceAll(tok, `"`, `""`))
		sb.WriteString(`"*`)
	}
	return sb.String()
}

func likeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

// Search runs an FTS5 MATCH query (prefix tokens, AND semantics) and falls
// back to a LIKE substring query when it yields no rows or errors.
func Search(ctx context.Context, d *sql.DB, query string, limit int) ([]File, error) {
	if limit <= 0 {
		limit = 50
	}
	expr := ftsExpr(query)
	if expr == "" {
		return nil, nil
	}
	res, err := queryFTS(ctx, d, expr, limit)
	if err == nil && len(res) > 0 {
		return res, nil
	}
	pattern := "%" + likeEscape(query) + "%"
	res2, err2 := queryLike(ctx, d, pattern, limit)
	if err2 != nil {
		if err != nil {
			return nil, err
		}
		return nil, err2
	}
	return res2, nil
}

func queryFTS(ctx context.Context, d *sql.DB, expr string, limit int) ([]File, error) {
	rows, err := d.QueryContext(ctx, ftsSQL, expr, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Path, &f.Name, &f.Size, &f.Ext, &f.MTime); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func queryLike(ctx context.Context, d *sql.DB, pattern string, limit int) ([]File, error) {
	rows, err := d.QueryContext(ctx, likeSQL, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Path, &f.Name, &f.Size, &f.Ext, &f.MTime); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Recent returns the most recently modified files, newest first.
func Recent(ctx context.Context, d *sql.DB, limit int) ([]File, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := d.QueryContext(ctx,
		`SELECT path, name, size, ext, mtime FROM files ORDER BY mtime DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Path, &f.Name, &f.Size, &f.Ext, &f.MTime); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

