package tmuxhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"

	"github.com/sodre90/cmux-bridge/internal/host"
)

// scrollbackCap is how many history rows a replay carries, matching the
// fixed-size scrollback block cmux ships (240 rows on a live pane) so the
// server's style-id canonicaliser, which treats a changing scrollback_rows
// as a resize, sees a constant once a pane has that much history.
const scrollbackCap = 240

// replayFormat is the pane state read in the same server turn as the
// screen text. One line, fields in stateFieldCount order.
const (
	replayFormat = "#{start_time}" + fieldSep +
		"#{window_id}" + fieldSep +
		"#{pane_width}" + fieldSep +
		"#{pane_height}" + fieldSep +
		"#{history_size}" + fieldSep +
		"#{cursor_x}" + fieldSep +
		"#{cursor_y}" + fieldSep +
		"#{cursor_flag}" + fieldSep +
		"#{alternate_on}" + fieldSep +
		"#{keypad_cursor_flag}" + fieldSep +
		"#{mouse_standard_flag}" + fieldSep +
		"#{mouse_button_flag}" + fieldSep +
		"#{mouse_all_flag}" + fieldSep +
		"#{mouse_sgr_flag}" + fieldSep +
		"#{bracket_paste_flag}"
	stateFieldCount = 15
)

// paneState is one replayFormat line, decoded.
type paneState struct {
	epoch                 int64
	windowID              string
	columns, rows         int
	history               int
	cursorX, cursorY      int
	cursorVisible         bool
	alternate             bool
	appCursor             bool
	mouseStandard         bool
	mouseButton, mouseAll bool
	mouseSGR              bool
	bracketPaste          bool
}

func parsePaneState(line string) (paneState, error) {
	f := strings.Split(line, fieldSep)
	if len(f) != stateFieldCount {
		return paneState{}, fmt.Errorf("%w: pane state has %d fields", host.ErrMalformed, len(f))
	}
	ints := make([]int, len(f))
	for i, s := range f {
		if i == 1 {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return paneState{}, fmt.Errorf("%w: pane state field %d = %q", host.ErrMalformed, i, s)
		}
		ints[i] = n
	}
	return paneState{
		epoch: int64(ints[0]), windowID: f[1],
		columns: ints[2], rows: ints[3], history: ints[4],
		cursorX: ints[5], cursorY: ints[6], cursorVisible: ints[7] == 1,
		alternate: ints[8] == 1, appCursor: ints[9] == 1,
		mouseStandard: ints[10] == 1, mouseButton: ints[11] == 1, mouseAll: ints[12] == 1,
		mouseSGR: ints[13] == 1, bracketPaste: ints[14] == 1,
	}, nil
}

// Replay snapshots a pane as a cmux.render-grid.v1 grid. State, scrollback
// and screen come from one tmux invocation so they describe the same
// moment: the format line, then up to scrollbackCap history rows (-E -1
// ends the capture at the last history line), then exactly pane_height
// screen rows. -N keeps trailing spaces so every row is a row; -e keeps
// the SGR sequences the parser turns into styles.
func (h *Host) Replay(ctx context.Context, surfaceID string) (host.Replay, error) {
	id, ok := decodeID(surfaceID)
	if !ok || id.kind != paneID {
		return host.Replay{}, &staleError{id: surfaceID}
	}
	target := id.target()
	out, err := h.tmux.Run(ctx,
		"display-message", "-p", "-t", target, "-F", replayFormat, ";",
		"capture-pane", "-e", "-p", "-N", "-t", target, "-S", "-"+strconv.Itoa(scrollbackCap), "-E", "-1", ";",
		"capture-pane", "-e", "-p", "-N", "-t", target,
	)
	if err != nil {
		return host.Replay{}, notFoundIfNoServer(err)
	}
	stateLine, body, _ := strings.Cut(string(out), "\n")
	state, err := parsePaneState(stateLine)
	if err != nil {
		return host.Replay{}, err
	}
	if state.epoch != id.epoch {
		return host.Replay{}, &staleError{id: surfaceID}
	}
	h.sizes.touched(state.windowID)
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	scrollback := min(state.history, scrollbackCap)
	if scrollback == 0 {
		lines = dropEmptyHistoryPlaceholder(lines, state.rows)
	}
	if len(lines) != scrollback+state.rows {
		return host.Replay{}, fmt.Errorf("%w: captured %d rows, want %d scrollback + %d screen",
			host.ErrMalformed, len(lines), scrollback, state.rows)
	}
	grid, err := json.Marshal(buildGrid(state, lines[:scrollback], lines[scrollback:], surfaceID))
	if err != nil {
		return host.Replay{}, err
	}
	return host.Replay{Grid: grid, Columns: state.columns, Rows: state.rows}, nil
}

// dropEmptyHistoryPlaceholder removes the one line tmux prints for a
// "-S -N -E -1" capture of a pane with no history at all: it clamps the
// end line to the screen's first row and echoes that (verified on 3.7c;
// N>0 history rows capture as exactly N lines).
func dropEmptyHistoryPlaceholder(lines []string, screenRows int) []string {
	if len(lines) == screenRows+1 {
		return lines[1:]
	}
	return lines
}

// renderGrid is the subset of cmux.render-grid.v1 the app reads plus the
// bookkeeping fields the server's style canonicaliser insists on seeing.
type renderGrid struct {
	Format          string       `json:"format"`
	Columns         int          `json:"columns"`
	Rows            int          `json:"rows"`
	Cursor          cursor       `json:"cursor"`
	Modes           []mode       `json:"modes"`
	RowSpans        []span       `json:"row_spans"`
	ScrollbackSpans []span       `json:"scrollback_spans"`
	ScrollbackRows  int          `json:"scrollback_rows"`
	Styles          []styleEntry `json:"styles"`
	ActiveScreen    string       `json:"active_screen"`
	Full            bool         `json:"full"`
	StateSeq        int64        `json:"state_seq"`
	RenderEpoch     string       `json:"render_epoch"`
	ClearedRows     []int        `json:"cleared_rows"`
	ScrolledRows    int          `json:"scrolled_rows"`
	Anchor          string       `json:"anchor"`
}

type cursor struct {
	Row      int    `json:"row"`
	Column   int    `json:"column"`
	Style    string `json:"style"`
	Visible  bool   `json:"visible"`
	Blinking bool   `json:"blinking"`
}

// mode is one DEC private mode in cmux's spelling.
type mode struct {
	ANSI bool `json:"ansi"`
	Code int  `json:"code"`
	On   bool `json:"on"`
}

type span struct {
	Row       int    `json:"row"`
	Column    int    `json:"column"`
	CellWidth int    `json:"cell_width"`
	StyleID   int    `json:"style_id"`
	Text      string `json:"text"`
}

type styleEntry struct {
	ID            int    `json:"id"`
	Foreground    string `json:"foreground,omitempty"`
	Background    string `json:"background,omitempty"`
	Bold          bool   `json:"bold"`
	Faint         bool   `json:"faint"`
	Italic        bool   `json:"italic"`
	Underline     bool   `json:"underline"`
	Inverse       bool   `json:"inverse"`
	Strikethrough bool   `json:"strikethrough"`
}

func buildGrid(state paneState, scrollback, screen []string, surfaceID string) renderGrid {
	styles := newStyleTable()
	g := renderGrid{
		Format:  "cmux.render-grid.v1",
		Columns: state.columns,
		Rows:    state.rows,
		Cursor: cursor{
			Row: state.cursorY, Column: state.cursorX, Style: "block", Visible: state.cursorVisible,
		},
		Modes: []mode{
			{Code: 1, On: state.appCursor},
			{Code: 1000, On: state.mouseStandard},
			{Code: 1002, On: state.mouseButton},
			{Code: 1003, On: state.mouseAll},
			{Code: 1006, On: state.mouseSGR},
			{Code: 2004, On: state.bracketPaste},
		},
		RowSpans:        []span{},
		ScrollbackSpans: []span{},
		ScrollbackRows:  len(scrollback),
		ActiveScreen:    "primary",
		Full:            true,
		RenderEpoch:     surfaceID,
		ClearedRows:     []int{},
		Anchor:          "viewport",
	}
	if state.alternate {
		g.ActiveScreen = "alternate"
	}
	for row, line := range scrollback {
		g.ScrollbackSpans = append(g.ScrollbackSpans, parseLine(line, row, styles)...)
	}
	for row, line := range screen {
		g.RowSpans = append(g.RowSpans, parseLine(line, row, styles)...)
	}
	g.Styles = styles.entries()
	return g
}

// styleTable numbers the styles a grid uses in first-seen order, id 0 the
// default; the server renumbers by content per socket afterwards.
type styleTable struct {
	ids  map[style]int
	list []style
}

func newStyleTable() *styleTable {
	return &styleTable{ids: map[style]int{{}: 0}, list: []style{{}}}
}

func (t *styleTable) id(s style) int {
	if id, ok := t.ids[s]; ok {
		return id
	}
	id := len(t.list)
	t.ids[s] = id
	t.list = append(t.list, s)
	return id
}

func (t *styleTable) entries() []styleEntry {
	out := make([]styleEntry, len(t.list))
	for i, s := range t.list {
		out[i] = styleEntry{
			ID: i, Foreground: s.fg, Background: s.bg,
			Bold: s.bold, Faint: s.faint, Italic: s.italic,
			Underline: s.underline, Inverse: s.inverse, Strikethrough: s.strike,
		}
	}
	return out
}

// parseLine turns one captured row into style runs. Runs of narrow
// characters share a span; a wide (two-cell) character is its own span,
// the only shape in which the app honours cell_width. Zero-width runes are
// dropped so columns stay aligned. Trailing blanks in the default style
// are omitted: the app already fills every unmentioned cell that way.
func parseLine(line string, row int, styles *styleTable) []span {
	var (
		out     []span
		current style
		col     int
		run     strings.Builder
		runCol  int
		runID   int
	)
	flush := func() {
		if run.Len() == 0 {
			return
		}
		text := run.String()
		width := col - runCol
		run.Reset()
		if runID == 0 {
			// Default-style blanks are what the app paints anyway; only a
			// styled space carries a background worth sending.
			trimmed := strings.TrimRight(text, " ")
			width -= len(text) - len(trimmed)
			text = trimmed
			if text == "" {
				return
			}
		}
		out = append(out, span{Row: row, Column: runCol, CellWidth: width, StyleID: runID, Text: text})
	}
	for i := 0; i < len(line); {
		if line[i] == 0x1b {
			var next style
			next, i = consumeEscape(line, i, current)
			if next != current {
				flush()
				current = next
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		i += size
		width := runewidth.RuneWidth(r)
		if width == 0 || r == utf8.RuneError && size == 1 {
			continue
		}
		id := styles.id(current)
		if run.Len() == 0 {
			runCol, runID = col, id
		}
		if width == 2 {
			flush()
			out = append(out, span{Row: row, Column: col, CellWidth: 2, StyleID: id, Text: string(r)})
			col += 2
			continue
		}
		run.WriteRune(r)
		col++
	}
	flush()
	return out
}

// consumeEscape skips the escape sequence starting at line[i] and returns
// the style after it plus the index past it. SGR updates the style; every
// other CSI, and any OSC (tmux emits OSC 8 hyperlinks), is skipped whole.
func consumeEscape(line string, i int, current style) (style, int) {
	if i+1 >= len(line) {
		return current, len(line)
	}
	switch line[i+1] {
	case '[':
		j := i + 2
		for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
			j++
		}
		if j >= len(line) {
			return current, j
		}
		if line[j] == 'm' {
			return applySGR(current, line[i+2:j]), j + 1
		}
		return current, j + 1
	case ']':
		j := i + 2
		for j < len(line) {
			if line[j] == 0x07 {
				return current, j + 1
			}
			if line[j] == 0x1b && j+1 < len(line) && line[j+1] == '\\' {
				return current, j + 2
			}
			j++
		}
		return current, j
	default:
		return current, i + 2
	}
}
