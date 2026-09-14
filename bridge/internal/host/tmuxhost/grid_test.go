package tmuxhost

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func spansOf(line string) ([]span, *styleTable) {
	styles := newStyleTable()
	return parseLine(line, 0, styles), styles
}

func TestParseLineGroupsRunsByStyleAndDropsDefaultBlanks(t *testing.T) {
	spans, styles := spansOf("ab \x1b[1;31mcd\x1b[0m   ef      ")
	if len(spans) != 3 {
		t.Fatalf("spans = %+v", spans)
	}
	if spans[0] != (span{Row: 0, Column: 0, CellWidth: 2, StyleID: 0, Text: "ab"}) {
		t.Errorf("first run = %+v", spans[0])
	}
	if spans[1].Column != 3 || spans[1].Text != "cd" || spans[1].StyleID != 1 || !styles.list[1].bold {
		t.Errorf("styled run = %+v (style %+v)", spans[1], styles.list[1])
	}
	if spans[2] != (span{Row: 0, Column: 5, CellWidth: 5, StyleID: 0, Text: "   ef"}) {
		t.Errorf("trailing run = %+v", spans[2])
	}
}

func TestParseLineGivesWideCharactersTheirOwnSpan(t *testing.T) {
	spans, _ := spansOf("a漢字b")
	want := []span{
		{Column: 0, CellWidth: 1, Text: "a"},
		{Column: 1, CellWidth: 2, Text: "漢"},
		{Column: 3, CellWidth: 2, Text: "字"},
		{Column: 5, CellWidth: 1, Text: "b"},
	}
	if len(spans) != len(want) {
		t.Fatalf("spans = %+v", spans)
	}
	for i := range want {
		if spans[i] != want[i] {
			t.Errorf("span %d = %+v, want %+v", i, spans[i], want[i])
		}
	}
}

func TestParseLineSkipsOSCAndForeignCSI(t *testing.T) {
	spans, styles := spansOf("\x1b[38;5;246m\x1b]8;id=z;https://x\x1b\\Guide\x1b[39m\x1b]8;;\x1b\\ \x1b[2Kend\x1b]0;title\x07")
	if len(spans) != 2 || spans[0].Text != "Guide" || spans[0].Column != 0 || spans[1].Text != " end" || spans[1].Column != 5 {
		t.Fatalf("spans = %+v", spans)
	}
	if styles.list[spans[0].StyleID].fg != "#949494" {
		t.Fatalf("style = %+v", styles.list[spans[0].StyleID])
	}
	if spans[1].StyleID != 0 {
		t.Fatalf("after 39 the run is default again: %+v", spans[1])
	}
}

func TestParseLineDropsZeroWidthAndInvalidBytes(t *testing.T) {
	spans, _ := spansOf("éx\xffy")
	if len(spans) != 1 || spans[0].Text != "exy" || spans[0].CellWidth != 3 {
		t.Fatalf("spans = %+v", spans)
	}
}

// The trust prompt Claude Code drew in a 100x30 tmux pane on the home
// server, captured with `display -p -F … ; capture-pane -e -p -N`.
func loadClaudeFixture(t *testing.T) (paneState, []string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/claude-trust-prompt.capture")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	// The fixture's first line is the human-readable probe format; the
	// state the parser reads is spelled here from the same values.
	state := paneState{
		epoch: 1789367814, windowID: "@0", columns: 100, rows: 30, history: 0,
		cursorX: 1, cursorY: 14, cursorVisible: false, bracketPaste: true,
	}
	return state, lines[1:]
}

func TestBuildGridFromClaudeCodeFixture(t *testing.T) {
	state, screen := loadClaudeFixture(t)
	if len(screen) != state.rows {
		t.Fatalf("fixture has %d screen rows, want %d", len(screen), state.rows)
	}
	g := buildGrid(state, nil, screen, "tmux-1789367814-p0")
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"format", "columns", "rows", "cursor", "modes", "row_spans", "scrollback_spans",
		"scrollback_rows", "styles", "active_screen", "full", "state_seq", "render_epoch", "cleared_rows",
		"scrolled_rows", "anchor"} {
		if _, ok := decoded[k]; !ok {
			t.Errorf("grid lacks %q", k)
		}
	}
	if string(decoded["styles"])[:8] != `[{"id":0` || strings.Contains(string(decoded["styles"])[:40], "background") {
		t.Fatalf("style 0 must be first and carry no colours: %s", decoded["styles"][:60])
	}
	text := func(row int) string {
		var b strings.Builder
		for _, s := range g.RowSpans {
			if s.Row == row {
				b.WriteString(s.Text)
			}
		}
		return b.String()
	}
	if got := text(2); !strings.Contains(got, "Accessing") || !strings.Contains(got, "workspace:") {
		t.Errorf("row 2 = %q", got)
	}
	if got := text(12); !strings.Contains(got, "Security guide") || strings.Contains(got, "https://") {
		t.Errorf("row 12 must keep the link text and drop the OSC: %q", got)
	}
	if got := text(14); !strings.Contains(got, "❯") || !strings.Contains(got, "No,") {
		t.Errorf("row 14 = %q", got)
	}
	// The yellow rule on row 1 is 100 cells of one 256-colour style.
	var rule *span
	for i := range g.RowSpans {
		if g.RowSpans[i].Row == 1 {
			rule = &g.RowSpans[i]
		}
	}
	if rule == nil || rule.CellWidth != 100 || g.Styles[rule.StyleID].Foreground != "#ffd700" {
		t.Errorf("rule span = %+v", rule)
	}
	if g.Cursor.Row != 14 || g.Cursor.Column != 1 || g.Cursor.Visible {
		t.Errorf("cursor = %+v", g.Cursor)
	}
	if g.Modes[5] != (mode{Code: 2004, On: true}) || g.Modes[0] != (mode{Code: 1}) {
		t.Errorf("modes = %+v", g.Modes)
	}
	if g.ActiveScreen != "primary" || g.ScrollbackRows != 0 || g.Anchor != "viewport" {
		t.Errorf("bookkeeping = %s %d %s", g.ActiveScreen, g.ScrollbackRows, g.Anchor)
	}
}

func TestParsePaneState(t *testing.T) {
	line := strings.Join([]string{"1789367814", "@3", "100", "30", "13", "23", "4", "1", "0", "1", "0", "1", "0", "1", "1"}, fieldSep)
	st, err := parsePaneState(line)
	if err != nil {
		t.Fatal(err)
	}
	if st.epoch != 1789367814 || st.windowID != "@3" || st.columns != 100 || st.rows != 30 || st.history != 13 ||
		st.cursorX != 23 || st.cursorY != 4 || !st.cursorVisible || st.alternate || !st.appCursor ||
		st.mouseStandard || !st.mouseButton || st.mouseAll || !st.mouseSGR || !st.bracketPaste {
		t.Fatalf("state = %+v", st)
	}
	if _, err := parsePaneState("1" + fieldSep + "@1"); err == nil {
		t.Fatal("short line must fail")
	}
}
