package tmuxhost

import "testing"

func TestApplySGRCoversTheAttributesTheAppRenders(t *testing.T) {
	s := applySGR(style{}, "1;31")
	if !s.bold || s.fg != palette[1] {
		t.Fatalf("1;31 = %+v", s)
	}
	s = applySGR(s, "38;5;208")
	if s.fg != "#ff8700" || !s.bold {
		t.Fatalf("38;5;208 = %+v", s)
	}
	s = applySGR(s, "48;2;10;200;30")
	if s.bg != "#0ac81e" {
		t.Fatalf("48;2 = %+v", s)
	}
	s = applySGR(s, "22;39")
	if s.bold || s.fg != "" || s.bg != "#0ac81e" {
		t.Fatalf("22;39 = %+v", s)
	}
	s = applySGR(s, "3;4;7;9;2")
	if !s.italic || !s.underline || !s.inverse || !s.strike || !s.faint {
		t.Fatalf("3;4;7;9;2 = %+v", s)
	}
	s = applySGR(s, "4:0")
	if s.underline {
		t.Fatalf("4:0 must clear underline: %+v", s)
	}
	s = applySGR(s, "38:2::1:2:3;4:3")
	if s.fg != "#010203" || !s.underline {
		t.Fatalf("colon forms = %+v", s)
	}
	s = applySGR(s, "58;5;1;97;104")
	if s.fg != palette[15] || s.bg != palette[12] {
		t.Fatalf("underline colour must be skipped, bright colours applied: %+v", s)
	}
	if applySGR(s, "0") != (style{}) || applySGR(s, "") != (style{}) {
		t.Fatal("0 and empty must reset")
	}
}

func TestPalette256Shape(t *testing.T) {
	if palette256(0) != palette[0] || palette256(15) != palette[15] {
		t.Fatal("first 16 are the ANSI palette")
	}
	if palette256(16) != "#000000" || palette256(231) != "#ffffff" || palette256(196) != "#ff0000" {
		t.Fatalf("cube: %s %s %s", palette256(16), palette256(231), palette256(196))
	}
	if palette256(232) != "#080808" || palette256(255) != "#eeeeee" {
		t.Fatalf("greys: %s %s", palette256(232), palette256(255))
	}
	if palette256(256) != "" || palette256(-1) != "" {
		t.Fatal("out of range is default")
	}
}
