// Package db provides SQLite persistence for the gofind file index.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

// File is a single indexed file row.
type File struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Ext   string `json:"ext"`
	MTime int64  `json:"mtime"` // unix seconds
}

const driverName = "sqlite"

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS files (
  id INTEGER PRIMARY KEY,
  path TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  size INTEGER NOT NULL DEFAULT 0,
  ext TEXT NOT NULL DEFAULT '',
  mtime INTEGER NOT NULL DEFAULT 0
)`,
	`CREATE INDEX IF NOT EXISTS idx_files_mtime ON files(mtime)`,
	`CREATE VIRTUAL TABLE IF NOT EXISTS files_fts USING fts5(
  name, path, content='files', content_rowid='id', tokenize='unicode61 remove_diacritics 2'
)`,
	`CREATE TRIGGER IF NOT EXISTS files_ai AFTER INSERT ON files BEGIN
  INSERT INTO files_fts(rowid, name, path) VALUES (new.id, new.name, new.path);
END`,
	`CREATE TRIGGER IF NOT EXISTS files_ad AFTER DELETE ON files BEGIN
  INSERT INTO files_fts(files_fts, rowid, name, path) VALUES ('delete', old.id, old.name, old.path);
END`,
	`CREATE TRIGGER IF NOT EXISTS files_au AFTER UPDATE ON files BEGIN
  INSERT INTO files_fts(files_fts, rowid, name, path) VALUES ('delete', old.id, old.name, old.path);
  INSERT INTO files_fts(rowid, name, path) VALUES (new.id, new.name, new.path);
END`,
}

var pragmas = []string{
	`PRAGMA journal_mode=WAL`,
	`PRAGMA synchronous=NORMAL`,
	`PRAGMA temp_store=MEMORY`,
	`PRAGMA cache_size=-64000`,
	`PRAGMA busy_timeout=5000`,
}

// Open creates the parent directory (0755) if needed, opens the database in
// WAL mode and applies the schema.
func Open(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("db: empty path")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("db: create directory %s: %w", dir, err)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(NORMAL)", "temp_store(MEMORY)", "cache_size(-64000)"} {
		q.Add("_pragma", p)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: q.Encode()}
	d, err := sql.Open(driverName, u.String())
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	// Single connection: WAL writer and the temp `seen` table must stay on one conn.
	d.SetMaxOpenConns(1)
	for _, pragma := range pragmas {
		if _, err := d.Exec(pragma); err != nil {
			d.Close()
			return nil, fmt.Errorf("db: %s: %w", pragma, err)
		}
	}
	for _, stmt := range schemaStatements {
		if _, err := d.Exec(stmt); err != nil {
			d.Close()
			return nil, fmt.Errorf("db: schema: %w", err)
		}
	}
	return d, nil
}

const upsertSQL = `INSERT INTO files (path, name, size, ext, mtime)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET
  name = excluded.name,
  size = excluded.size,
  ext = excluded.ext,
  mtime = excluded.mtime
WHERE files.size <> excluded.size OR files.mtime <> excluded.mtime`

// BatchStats reports the outcome of UpsertBatch. Rows already present with
// unchanged size and mtime count as Skipped.
type BatchStats struct {
	New     int
	Updated int
	Skipped int
}

// UpsertBatch inserts or updates rows in one transaction. newCount is inferred
// from paths that did not exist before the batch; RowsAffected (insert branch:
// 1, update branch: 1 when size/mtime changed, 0 otherwise) yields updated and
// skipped counts. Paths within one batch must be unique (a directory walk
// guarantees this).
func UpsertBatch(ctx context.Context, d *sql.DB, files []File) (BatchStats, error) {
	var st BatchStats
	if len(files) == 0 {
		return st, nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback()

	exists := make(map[string]struct{}, len(files))
	const chunk = 900 // stay well below SQLITE_MAX_VARIABLE_NUMBER
	for start := 0; start < len(files); start += chunk {
		end := min(start+chunk, len(files))
		var sb strings.Builder
		sb.WriteString("SELECT path FROM files WHERE path IN (")
		for i := start; i < end; i++ {
			if i > start {
				sb.WriteByte(',')
			}
			sb.WriteByte('?')
		}
		sb.WriteString(")")
		args := make([]any, 0, end-start)
		for i := start; i < end; i++ {
			args = append(args, files[i].Path)
		}
		rows, err := tx.QueryContext(ctx, sb.String(), args...)
		if err != nil {
			return st, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return st, err
			}
			exists[p] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return st, err
		}
		rows.Close()
	}

	stmt, err := tx.PrepareContext(ctx, upsertSQL)
	if err != nil {
		return st, err
	}
	defer stmt.Close()
	var affected int64
	for i := range files {
		f := files[i]
		r, err := stmt.ExecContext(ctx, f.Path, f.Name, f.Size, f.Ext, f.MTime)
		if err != nil {
			return st, fmt.Errorf("db: upsert %s: %w", f.Path, err)
		}
		n, err := r.RowsAffected()
		if err != nil {
			return st, err
		}
		affected += n
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	st.New = len(files) - len(exists)
	st.Updated = int(affected) - st.New
	st.Skipped = len(exists) - st.Updated
	return st, nil
}

// SeenCreate creates the temp table used by PruneNotSeen (same connection).
func SeenCreate(ctx context.Context, d *sql.DB) error {
	_, err := d.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS seen (path TEXT PRIMARY KEY)`)
	return err
}

// SeenInsert records the paths seen during this run.
func SeenInsert(ctx context.Context, d *sql.DB, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO seen (path) VALUES (?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range paths {
		if _, err := stmt.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("db: seen insert %s: %w", p, err)
		}
	}
	return tx.Commit()
}

// PruneNotSeen deletes rows whose path was not seen in this run.
func PruneNotSeen(ctx context.Context, d *sql.DB) (int64, error) {
	r, err := d.ExecContext(ctx, `DELETE FROM files WHERE path NOT IN (SELECT path FROM seen)`)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// Count returns the total number of indexed files.
func Count(ctx context.Context, d *sql.DB) (int64, error) {
	var n int64
	err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&n)
	return n, err
}

// GetByPath returns a single file row.
func GetByPath(ctx context.Context, d *sql.DB, path string) (File, error) {
	var f File
	err := d.QueryRowContext(ctx,
		`SELECT path, name, size, ext, mtime FROM files WHERE path = ?`, path).
		Scan(&f.Path, &f.Name, &f.Size, &f.Ext, &f.MTime)
	if err == sql.ErrNoRows {
		return File{}, fmt.Errorf("db: %s: %w", path, sql.ErrNoRows)
	}
	return f, err
}

// DeleteByPath removes a file row (the AFTER DELETE trigger updates files_fts).
func DeleteByPath(ctx context.Context, d *sql.DB, path string) (bool, error) {
	r, err := d.ExecContext(ctx, `DELETE FROM files WHERE path = ?`, path)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
