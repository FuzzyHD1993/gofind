// Package index walks a directory tree and upserts file metadata into SQLite.
package index

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gofind/internal/db"
)

const (
	batchSize     = 1000
	progressEvery = 2000
)

// Options configures an index run.
type Options struct {
	Root   string
	DBPath string
	Prune  bool
	Quiet  bool
}

// Result summarises one index run.
type Result struct {
	Total   int           `json:"total"`
	New     int           `json:"new"`
	Updated int           `json:"updated"`
	Skipped int           `json:"skipped"`
	Pruned  int64         `json:"pruned"`
	Elapsed time.Duration `json:"-"`
}

// skipDirs are directory base names never descended into.
var skipDirs = map[string]bool{
	".git":                      true,
	"node_modules":              true,
	"@eaDir":                    true,
	"#recycle":                  true,
	"System Volume Information": true,
	".Trash-1000":               true,
	".snapshot":                 true,
	"lost+found":                true,
}

// skipNames are file base names never indexed.
var skipNames = map[string]bool{
	".DS_Store":  true,
	"Thumbs.db":  true,
	"desktop.ini": true,
}

// skipExts are file extensions (lowercase, with dot) never indexed.
var skipExts = map[string]bool{
	".tmp":        true,
	".part":       true,
	".!qb":        true,
	".crdownload": true,
}

// Run walks opts.Root (no symlink following), upserts rows in batches of 1000
// and prints progress to stderr every 2000 files unless opts.Quiet. It never
// deletes rows; with opts.Prune it removes paths not seen in this run.
func Run(ctx context.Context, opts Options) (Result, error) {
	var res Result
	start := time.Now()
	if opts.Root == "" {
		return res, fmt.Errorf("index: empty root")
	}
	d, err := db.Open(opts.DBPath)
	if err != nil {
		return res, err
	}
	defer d.Close()

	if opts.Prune {
		if err := db.SeenCreate(ctx, d); err != nil {
			return res, fmt.Errorf("index: seen table: %w", err)
		}
	}

	var batch []db.File
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		st, err := db.UpsertBatch(ctx, d, batch)
		if err != nil {
			return fmt.Errorf("index: batch: %w", err)
		}
		res.New += st.New
		res.Updated += st.Updated
		res.Skipped += st.Skipped
		if opts.Prune {
			paths := make([]string, len(batch))
			for i := range batch {
				paths[i] = batch[i].Path
			}
			if err := db.SeenInsert(ctx, d, paths); err != nil {
				return fmt.Errorf("index: seen insert: %w", err)
			}
		}
		batch = batch[:0]
		return nil
	}

	err = filepath.WalkDir(opts.Root, func(path string, entry fs.DirEntry, werr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if werr != nil {
			// Log read/stat errors and keep walking the rest of the tree.
			fmt.Fprintf(os.Stderr, "gofind: %s: %v\n", path, werr)
			return nil
		}
		if entry.IsDir() {
			// The user's explicit root is always walked.
			if path != opts.Root && skipDirs[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		typ := entry.Type()
		if !typ.IsRegular() && typ&fs.ModeSymlink == 0 {
			return nil // sockets, fifos, devices
		}
		name := entry.Name()
		if skipNames[name] || strings.HasPrefix(name, "._") || skipExts[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		info, ierr := entry.Info()
		if ierr != nil {
			fmt.Fprintf(os.Stderr, "gofind: %s: %v\n", path, ierr)
			return nil
		}
		if info.Size() == 0 {
			return nil
		}
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
		batch = append(batch, db.File{
			Path:  path,
			Name:  name,
			Size:  info.Size(),
			Ext:   ext,
			MTime: info.ModTime().Unix(),
		})
		res.Total++
		if res.Total%progressEvery == 0 && !opts.Quiet {
			printProgress(os.Stderr, res, time.Since(start))
		}
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("index: walk %s: %w", opts.Root, err)
	}
	if err := flush(); err != nil {
		return res, err
	}
	if opts.Prune {
		n, err := db.PruneNotSeen(ctx, d)
		if err != nil {
			return res, fmt.Errorf("index: prune: %w", err)
		}
		res.Pruned = n
	}
	res.Elapsed = time.Since(start)
	return res, nil
}

func printProgress(w io.Writer, res Result, elapsed time.Duration) {
	rate := 0.0
	if elapsed > 0 {
		rate = float64(res.Total) / elapsed.Seconds()
	}
	fmt.Fprintf(w, "index: seen=%d new=%d updated=%d skipped=%d elapsed=%dms rate=%.0f files/s\n",
		res.Total, res.New, res.Updated, res.Skipped, elapsed.Milliseconds(), rate)
}
