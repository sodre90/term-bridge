package tmuxhost

import "testing"

func TestIDsRoundTripAndStayURLSafe(t *testing.T) {
	for _, tc := range []struct{ tmuxID, wire string }{
		{"@3", "tmux-1789367814-w3"},
		{"%12", "tmux-1789367814-p12"},
	} {
		got := encodeID(1789367814, tc.tmuxID)
		if got != tc.wire {
			t.Errorf("encode %s = %s, want %s", tc.tmuxID, got, tc.wire)
		}
		id, ok := decodeID(got)
		if !ok || id.target() != tc.tmuxID || id.epoch != 1789367814 {
			t.Errorf("decode %s = %+v, %v", got, id, ok)
		}
		if !validID(got) {
			t.Errorf("%s must be valid", got)
		}
	}
	for _, bad := range []string{"", "@3", "%5", "tmux-x-w3", "tmux-1-q3", "tmux-1-w", "0B1D3CA1-7E2A-4E7B-9E2F-0A1B2C3D4E5F", "tmux-1-w3/../x"} {
		if validID(bad) {
			t.Errorf("%q must not be valid", bad)
		}
		if _, ok := decodeID(bad); ok {
			t.Errorf("%q must not decode", bad)
		}
	}
}
