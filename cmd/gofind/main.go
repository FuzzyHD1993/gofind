// Command gofind indexes a NAS directory tree and searches it fast.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"gofind/internal/db"
	"gofind/internal/index"
	"gofind/internal/search"
	"gofind/internal/ui"
)

const usageText = `gofind — index and search a NAS file tree

Usage:
  gofind index [-root DIR] [-db PATH] [-prune] [-quiet]
  gofind search <query...> [-db PATH] [-limit N] [-json]
  gofind tui [-db PATH]

Flags:
  index  -root DIR    directory to index (default /mnt/nas/root/FuzzyHD)
         -db PATH     database path (default ~/.local/share/gofind/gofind.db)
         -prune       remove entries not seen in this run
         -quiet       no progress output on stderr
  search -db PATH     database path
         -limit N     max results (default 50)
         -json        print results as a JSON array
  tui    -db PATH     database path
`

const defaultRoot = "/mnt/nas/root/FuzzyHD"

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "gofind.db"
	}
	return filepath.Join(home, ".local", "share", "gofind", "gofind.db")
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "index":
		err = cmdIndex(ctx, os.Args[2:])
	case "search":
		err = cmdSearch(ctx, os.Args[2:])
	case "tui":
		err = cmdTui(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "gofind: unknown command %q\n\n%s", os.Args[1], usageText)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(usageText)
			return
		}
		fmt.Fprintln(os.Stderr, "gofind:", err)
		os.Exit(1)
	}
}

func cmdIndex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	root := fs.String("root", defaultRoot, "directory to index")
	dbPath := fs.String("db", defaultDBPath(), "database path")
	prune := fs.Bool("prune", false, "remove entries not seen in this run")
	quiet := fs.Bool("quiet", false, "suppress progress output on stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("index: unexpected arguments: %v", fs.Args())
	}
	res, err := index.Run(ctx, index.Options{
		Root:   *root,
		DBPath: *dbPath,
		Prune:  *prune,
		Quiet:  *quiet,
	})
	if err != nil {
		return err
	}
	line := fmt.Sprintf("index: %d files: %d new, %d updated, %d skipped in %s",
		res.Total, res.New, res.Updated, res.Skipped, res.Elapsed.Round(time.Millisecond))
	if *prune {
		line += fmt.Sprintf(", %d pruned", res.Pruned)
	}
	fmt.Println(line)
	return nil
}

type searchOpts struct {
	dbPath string
	limit  int
	asJSON bool
	query  []string
}

// parseSearchArgs accepts flags anywhere around the query words
// (e.g. `gofind search matrix -db x.db -limit 5 -json`).
func parseSearchArgs(args []string) (searchOpts, error) {
	opts := searchOpts{dbPath: defaultDBPath(), limit: 50}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-db":
			i++
			if i >= len(args) {
				return opts, errors.New("search: -db requires a path")
			}
			opts.dbPath = args[i]
		case "-limit":
			i++
			if i >= len(args) {
				return opts, errors.New("search: -limit requires a number")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n <= 0 {
				return opts, fmt.Errorf("search: invalid -limit %q", args[i])
			}
			opts.limit = n
		case "-json":
			opts.asJSON = true
		case "-h", "--help":
			return opts, flag.ErrHelp
		default:
			if strings.HasPrefix(args[i], "-") {
				return opts, fmt.Errorf("search: unknown flag %s", args[i])
			}
			opts.query = append(opts.query, args[i])
		}
	}
	return opts, nil
}

func cmdSearch(ctx context.Context, args []string) error {
	opts, err := parseSearchArgs(args)
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(opts.query, " "))
	if query == "" {
		return errors.New("search: empty query")
	}
	d, err := db.Open(opts.dbPath)
	if err != nil {
		return err
	}
	defer d.Close()
	files, err := search.Search(ctx, d, query, opts.limit)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	if opts.asJSON {
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(files)
	}
	for _, f := range files {
		fmt.Printf("%s\t%d\t%s\n", ui.HumanSize(f.Size), f.MTime, f.Path)
	}
	return nil
}

func cmdTui(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := tea.NewProgram(ui.New(*dbPath), tea.WithAltScreen())
	_, err := p.Run()
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
