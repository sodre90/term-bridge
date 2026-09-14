package cmuxhost

import "testing"

func TestClassifyKind(t *testing.T) {
	cases := map[string]string{
		"Build options trading system": "agent",
		"~/prj/log-search":             "terminal",
		"u@host:~/prj/trading":         "terminal",
		"/Users/u/prj/x":               "terminal",
	}
	for title, want := range cases {
		if got := classifyKind(title); got != want {
			t.Errorf("classifyKind(%q)=%q want %q", title, got, want)
		}
	}
}

func TestClassifyAttention(t *testing.T) {
	cases := map[string]string{
		"Claude needs your permission":     "permission",
		"Claude is waiting for your input": "input",
		"CODEX NEEDS YOUR PERMISSION":      "permission", // case-insensitive
		"All done. Summary of the work…":   "",
		"":                                 "",
	}
	for preview, want := range cases {
		if got := classifyAttention(preview); got != want {
			t.Errorf("classifyAttention(%q)=%q want %q", preview, got, want)
		}
	}
}

func TestCleanTitleStripsGlyph(t *testing.T) {
	if got := cleanTitle("⠂ Build price comparison"); got != "Build price comparison" {
		t.Fatalf("cleanTitle glyph strip failed: %q", got)
	}
	if got := cleanTitle("Plain title"); got != "Plain title" {
		t.Fatalf("cleanTitle altered plain title: %q", got)
	}
}
