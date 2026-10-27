// Package ui contains the bubbletea TUI for gofind.
package ui

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"gofind/internal/db"
	"gofind/internal/search"
)

const (
	debounceDelay = 80 * time.Millisecond
	tuiLimit      = 500
	recentLimit   = 200
)

var (
	nameStyle     = lipgloss.NewStyle()
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	metaStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	pathStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

type resultMsg struct {
	seq     int
	results []search.File
	err     error
}

type recentMsg struct {
	results []search.File
	err     error
}

type dbReadyMsg struct {
	db  *sql.DB
	err error
}

// Model is the bubbletea model for the gofind TUI.
type Model struct {
	dbPath   string
	db       *sql.DB
	dbErr    error
	input    textinput.Model
	vp       viewport.Model
	results  []search.File
	cursor   int
	width    int
	height   int
	searching bool
	err      error
	lastQuery string
	seq      int
	ready    bool
	recentDone bool
}

// New returns the initial TUI model for the given database path.
func New(dbPath string) Model {
	in := textinput.New()
	in.Placeholder = "search…"
	in.CharLimit = 256
	in.Focus()
	return Model{dbPath: dbPath, input: in}
}

// Init opens the database asynchronously and blinks the cursor.
func (m Model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, loadDB(m.dbPath))
}

func loadDB(path string) tea.Cmd {
	return func() tea.Msg {
		d, err := db.Open(path)
		return dbReadyMsg{db: d, err: err}
	}
}

func debounceCmd(seq int) tea.Cmd {
	return tea.Tick(debounceDelay, func(time.Time) tea.Msg {
		return tickMsg{seq: seq}
	})
}

func doSearchCmd(d *sql.DB, seq int, query string) tea.Cmd {
	return func() tea.Msg {
		if d == nil {
			return resultMsg{seq: seq, err: fmt.Errorf("database not ready")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := search.Search(ctx, d, query, tuiLimit)
		return resultMsg{seq: seq, results: res, err: err}
	}
}

func recentCmd(d *sql.DB) tea.Cmd {
	return func() tea.Msg {
		if d == nil {
			return recentMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := search.Recent(ctx, d, recentLimit)
		return recentMsg{results: res, err: err}
	}
}

type tickMsg struct{ seq int }

// Update handles key presses, resizes and async messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.input.Width = max(0, m.width-2)
		m.vp.Width = m.width
		m.vp.Height = max(0, m.height-2) // input line + status line
		return m, nil

	case dbReadyMsg:
		m.db, m.dbErr = msg.db, msg.err
		if m.dbErr != nil {
			return m, nil
		}
		if !m.recentDone {
			m.recentDone = true
			return m, recentCmd(m.db)
		}
		return m, nil

	case recentMsg:
		// Only seed the list if the user has not started typing.
		if msg.err == nil && m.input.Value() == "" && m.lastQuery == "" && len(m.results) == 0 {
			m.results = msg.results
			m.cursor = 0
		}
		return m, nil

	case resultMsg:
		if msg.seq == m.seq { // drop stale results from superseded queries
			m.searching = false
			m.err = msg.err
			m.results = msg.results
			m.cursor = 0
		}
		return m, nil

	case tickMsg:
		if msg.seq != m.seq {
			return m, nil // a newer keystroke reset the debounce window
		}
		if m.input.Value() == m.lastQuery {
			return m, nil
		}
		m.lastQuery = m.input.Value()
		m.seq++
		return m, doSearchCmd(m.db, m.seq, m.lastQuery)

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}
	return m, nil
}

// handleKey resolves the key priority: quit/clear, then navigation, then text
// input. While the query box is empty the list is in browse mode (q quits,
// j/k move); with text entered those characters go into the query, and
// arrow keys still navigate.
func (m Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	key := msg.String()
	switch key {
	case "ctrl+c":
		return tea.Quit
	case "esc":
		m.input.SetValue("")
		m.results = nil
		m.cursor = 0
		m.lastQuery = ""
		m.searching = false
		m.seq++
		return nil
	case "enter":
		if m.cursor >= 0 && m.cursor < len(m.results) {
			openDirOf(m.results[m.cursor].Path)
		}
		return nil
	case "up":
		return m.moveCursor(-1)
	case "down":
		return m.moveCursor(1)
	case "q":
		if m.input.Value() == "" {
			return tea.Quit
		}
	case "k":
		if m.input.Value() == "" {
			return m.moveCursor(-1)
		}
	case "j":
		if m.input.Value() == "" {
			return m.moveCursor(1)
		}
	}
	// Everything else goes into the query box; on change, restart debounce.
	before := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() == before {
		return cmd
	}
	m.cursor = 0
	m.seq++
	if m.input.Value() == "" {
		m.results = nil
		m.lastQuery = ""
		m.searching = false
		return cmd
	}
	m.searching = true
	return tea.Batch(cmd, debounceCmd(m.seq))
}

func (m *Model) moveCursor(delta int) tea.Cmd {
	if len(m.results) == 0 {
		return nil
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.results) {
		m.cursor = len(m.results) - 1
	}
	return nil
}

// openDirOf opens the containing directory with xdg-open, detached, ignoring
// any errors.
func openDirOf(path string) {
	cmd := exec.Command("xdg-open", filepath.Dir(path))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err == nil && cmd.Process != nil {
		go func() { _ = cmd.Wait() }() // reap so the child does not become a zombie
	}
}

// View renders the query box, status line and scrolling result list.
func (m Model) View() string {
	var status string
	switch {
	case m.dbErr != nil:
		status = errStyle.Render("db error: " + m.dbErr.Error())
	case m.searching:
		status = metaStyle.Render("searching…")
	case m.err != nil:
		status = errStyle.Render("error: " + m.err.Error())
	case m.db == nil:
		status = metaStyle.Render("loading…")
	case len(m.results) == 0:
		status = metaStyle.Render("no results")
	default:
		status = fmt.Sprintf("%s%d/%d results", metaStyle.Render(""), len(m.results), tuiLimit)
	}
	status = metaStyle.Render("") + status + metaStyle.Render("  ·  enter: open dir · esc: clear · q: quit")

	m.vp.SetContent(m.renderList())
	m.keepCursorVisible()

	return lipgloss.JoinVertical(lipgloss.Left, m.input.View(), status, m.vp.View())
}

func (m Model) renderList() string {
	var sb strings.Builder
	for i, f := range m.results {
		marker := "  "
		nameSt, pathSt := nameStyle, pathStyle
		if i == m.cursor {
			marker = "› "
			nameSt = selectedStyle
			pathSt = metaStyle
		}
		line := lipgloss.JoinHorizontal(lipgloss.Left,
			marker,
			nameSt.Render(f.Name),
			"  ",
			metaStyle.Render(HumanSize(f.Size)),
			"  ",
			metaStyle.Render(date(f.MTime)),
			"  ",
			pathSt.Render(f.Path),
		)
		if m.width > 0 {
			line = lipgloss.NewStyle().MaxWidth(m.width).Render(line)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

// keepCursorVisible scrolls the viewport so the selected row stays on screen.
func (m *Model) keepCursorVisible() {
	vis := m.vp.Height
	if vis <= 0 {
		return
	}
	if m.cursor < m.vp.YOffset {
		m.vp.SetYOffset(m.cursor)
	} else if m.cursor >= m.vp.YOffset+vis {
		m.vp.SetYOffset(m.cursor - vis + 1)
	}
}

// HumanSize formats a byte count compactly, e.g. "512B", "1.2G".
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

func date(mtime int64) string {
	return time.Unix(mtime, 0).Format("2006-01-02")
}
