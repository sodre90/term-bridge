package server

import (
	"bytes"
	"encoding/json"
	"slices"
)

// rowDelta remembers the bytes of each visible row's spans as one socket last
// sent them, so an output frame can carry only the rows that differ.
//
// Measured on a busy agent pane (cmux-app-bly, 2026-09-14), the visible
// screen was ~50 KB of spans re-sent four times a second because a spinner
// changed one or two of its 65 rows. A row is kept only while its spans'
// bytes are identical, and the app resolves style ids against the styles
// table of the frame it is completing, so a kept row means exactly what the
// frame would have said had it carried it -- whatever happened to the style
// table meanwhile. That is why this needs no list of grid states to refuse:
// a resize changes every reflowed row's bytes, and rows that fell off the
// bottom arrive as changed-and-empty.
type rowDelta struct{ rows map[int][]byte }

func newRowDelta() *rowDelta { return &rowDelta{rows: map[int][]byte{}} }

// cut returns the spans of the rows that differ from the frame before, with
// those rows' numbers in ascending order, and records this frame. A row that
// no longer has any spans is listed too, with none, which is how the app
// learns to clear it. ok is false when spans is not an array of objects each
// carrying an integer row: the block must then go out whole, and the record
// is reset so the next frame starts from what the app was actually sent.
func (d *rowDelta) cut(spans json.RawMessage) (partial json.RawMessage, changed []int, ok bool) {
	var list []json.RawMessage
	if err := json.Unmarshal(spans, &list); err != nil {
		return d.reset()
	}
	rowOf := make([]int, len(list))
	current := make(map[int][]byte, len(d.rows))
	for i, span := range list {
		var at struct {
			Row *int `json:"row"`
		}
		if err := json.Unmarshal(span, &at); err != nil || at.Row == nil {
			return d.reset()
		}
		rowOf[i] = *at.Row
		current[*at.Row] = append(append(current[*at.Row], span...), ',')
	}
	for row, now := range current {
		if before, seen := d.rows[row]; !seen || !bytes.Equal(before, now) {
			changed = append(changed, row)
		}
	}
	for row := range d.rows {
		if _, still := current[row]; !still {
			changed = append(changed, row)
		}
	}
	d.rows = current
	if len(changed) == 0 {
		return nil, nil, true
	}
	slices.Sort(changed)
	buf := make([]byte, 0, len(spans)/8)
	buf = append(buf, '[')
	for i, span := range list {
		if _, moved := slices.BinarySearch(changed, rowOf[i]); !moved {
			continue
		}
		if len(buf) > 1 {
			buf = append(buf, ',')
		}
		buf = append(buf, span...)
	}
	return append(buf, ']'), changed, true
}

func (d *rowDelta) reset() (json.RawMessage, []int, bool) {
	d.rows = map[int][]byte{}
	return nil, nil, false
}
