package server

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

const twoRows = `{"cursor":{"row":1},"row_spans":[{"row":0,"column":0,"style_id":0,"text":"prompt"},{"row":1,"column":0,"style_id":1,"text":"spin |"}]}`

func TestARowThatChangedIsSentAloneAndNamed(t *testing.T) {
	d := newDeltaEncoder(true)
	d.strip(json.RawMessage(twoRows))
	got, omitted, rows := d.strip(json.RawMessage(strings.Replace(twoRows, "spin |", "spin /", 1)))
	if !slices.Equal(rows, []int{1}) || len(omitted) != 0 {
		t.Fatalf("rows_changed = %v, unchanged = %v", rows, omitted)
	}
	if strings.Contains(string(got), "prompt") || !strings.Contains(string(got), "spin /") {
		t.Fatalf("only the changed row's spans belong in the frame: %s", got)
	}
	if !strings.Contains(string(got), `"cursor"`) {
		t.Fatalf("the rest of the grid must stay: %s", got)
	}
}

func TestAnUnchangedScreenIsNamedUnchangedRatherThanSent(t *testing.T) {
	d := newDeltaEncoder(true)
	d.strip(json.RawMessage(twoRows))
	got, omitted, rows := d.strip(json.RawMessage(strings.Replace(twoRows, `"row":1}`, `"row":0}`, 1)))
	if rows != nil || !slices.Equal(omitted, []string{"row_spans"}) {
		t.Fatalf("rows_changed = %v, unchanged = %v", rows, omitted)
	}
	if strings.Contains(string(got), "row_spans") || !strings.Contains(string(got), `"cursor":{"row":0}`) {
		t.Fatalf("row_spans must be left out and the cursor kept: %s", got)
	}
}

func TestARowThatEmptiedIsNamedWithNoSpans(t *testing.T) {
	d := newDeltaEncoder(true)
	d.strip(json.RawMessage(twoRows))
	got, _, rows := d.strip(json.RawMessage(`{"row_spans":[{"row":0,"column":0,"style_id":0,"text":"prompt"}]}`))
	if !slices.Equal(rows, []int{1}) {
		t.Fatalf("an emptied row must be listed, got %v", rows)
	}
	if !strings.Contains(string(got), `"row_spans":[]`) {
		t.Fatalf("an emptied row carries no spans: %s", got)
	}
	// Then it stays empty without being mentioned again.
	if _, omitted, rows := d.strip(json.RawMessage(`{"row_spans":[{"row":0,"column":0,"style_id":0,"text":"prompt"}]}`)); rows != nil || !slices.Equal(omitted, []string{"row_spans"}) {
		t.Fatalf("rows_changed = %v, unchanged = %v", rows, omitted)
	}
}

func TestSpansWithoutARowSendTheScreenWholeAndForgetIt(t *testing.T) {
	d := newDeltaEncoder(true)
	d.strip(json.RawMessage(twoRows))
	bad := json.RawMessage(`{"row_spans":[{"column":0,"text":"?"}]}`)
	got, omitted, rows := d.strip(bad)
	if !bytes.Equal(got, bad) || omitted != nil || rows != nil {
		t.Fatalf("an unreadable block must go out whole: %s / %v / %v", got, omitted, rows)
	}
	// The next readable frame is a first frame again: every row is new.
	_, _, rows = d.strip(json.RawMessage(twoRows))
	if !slices.Equal(rows, []int{0, 1}) {
		t.Fatalf("after a reset every row is changed, got %v", rows)
	}
}

func TestASocketThatDidNotAskNeverGetsPartialRows(t *testing.T) {
	d := newDeltaEncoder(false)
	d.strip(json.RawMessage(twoRows))
	got, omitted, rows := d.strip(json.RawMessage(twoRows))
	if rows != nil || len(omitted) != 0 || !strings.Contains(string(got), "prompt") {
		t.Fatalf("rows_changed = %v, unchanged = %v, grid = %s", rows, omitted, got)
	}
}

// Row 0 never changes and row 1 changes on every replay: the shape of an agent
// pane whose spinner ticks under a fixed prompt.
const fakeSpinnerScript = `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
case "$2" in
  mobile.terminal.replay)
    n=$(grep -c 'mobile.terminal.replay' "$CMUX_FAKE_LOG")
    cat <<JSON
{"columns":80,"rows":24,"seq":0,"surface_id":"S","workspace_id":"W","render_grid":{"format":"cmux.render-grid.v1","columns":80,"rows":24,"scrollback_rows":0,"row_spans":[{"row":0,"column":0,"style_id":0,"text":"prompt"},{"row":1,"column":0,"style_id":0,"text":"tick-$n"}]}}
JSON
    ;;
  *) echo '{"ok":true}' ;;
esac
`

func TestOutputFramesCarryOnlyTheRowsThatChanged(t *testing.T) {
	srv, deviceID, secret := newEncryptedTerminalServer(t, fakeSpinnerScript)
	c, _ := dialTerminal(t, srv.URL, "/terminal/SURF1?delta=1&rows=1", "relay-secret", deviceID)
	defer c.Close()

	_, replay := readFrame(t, c, secret, 0, false)
	if replay.Type != "replay" || replay.RowsChanged != nil || !strings.Contains(string(replay.Grid), "prompt") {
		t.Fatalf("the replay must be whole and unqualified: %+v", replay)
	}
	_, out := readFrame(t, c, secret, 1, false)
	if out.Type != "output" || !slices.Equal(out.RowsChanged, []int{1}) {
		t.Fatalf("want an output frame naming row 1, got %+v", out)
	}
	if strings.Contains(string(out.Grid), "prompt") || !strings.Contains(string(out.Grid), "tick-") {
		t.Fatalf("only the ticking row belongs in the frame: %s", out.Grid)
	}
}

func TestOutputFramesStayWholeWithoutTheRowsHandshake(t *testing.T) {
	srv, deviceID, secret := newEncryptedTerminalServer(t, fakeSpinnerScript)
	c, _ := dialTerminal(t, srv.URL, "/terminal/SURF1?delta=1", "relay-secret", deviceID)
	defer c.Close()
	readFrame(t, c, secret, 0, false)
	_, out := readFrame(t, c, secret, 1, false)
	if out.RowsChanged != nil || !strings.Contains(string(out.Grid), "prompt") {
		t.Fatalf("an app that did not ask must get the whole screen: %+v", out)
	}
}

// The Kotlin side's RowDeltaFixtureTest holds these three strings: the two
// whole screens, and what the bridge sends for the second once it has seen the
// first. If this fails after a bridge change, regenerate the Kotlin constants
// from what it prints.
func TestTheCrossLanguageRowFixtureIsUnchanged(t *testing.T) {
	const first = `{"columns":6,"rows":3,"styles":[{"id":0},{"id":1,"bold":true}],"row_spans":[` +
		`{"row":0,"column":0,"style_id":0,"text":"$ ls","cell_width":1},` +
		`{"row":1,"column":0,"style_id":1,"text":"a.txt","cell_width":1},` +
		`{"row":2,"column":0,"style_id":0,"text":"spin |","cell_width":1}]}`
	const second = `{"columns":6,"rows":3,"styles":[{"id":0},{"id":1,"bold":true}],"row_spans":[` +
		`{"row":0,"column":0,"style_id":0,"text":"$ ls","cell_width":1},` +
		`{"row":2,"column":0,"style_id":0,"text":"spin /","cell_width":1}]}`
	d := newDeltaEncoder(true)
	d.strip(json.RawMessage(first))
	got, omitted, rows := d.strip(json.RawMessage(second))
	const want = `{"columns":6,"row_spans":[{"row":2,"column":0,"style_id":0,"text":"spin /","cell_width":1}],"rows":3}`
	if string(got) != want || !slices.Equal(omitted, []string{"styles"}) || !slices.Equal(rows, []int{1, 2}) {
		t.Fatalf("fixture changed; regenerate the Kotlin constants from:\ngrid: %s\nunchanged: %v\nrows_changed: %v", got, omitted, rows)
	}
}
