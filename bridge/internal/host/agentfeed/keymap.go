package agentfeed

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sodre90/term-bridge/internal/host"
	"github.com/sodre90/term-bridge/internal/wire"
)

// option is one numbered choice of Claude Code's prompt as it stands on
// screen: "❯ 1. Yes" gives key "1", label "Yes".
type option struct {
	key   string
	label string
}

// optionLine matches a prompt choice: an optional selection marker, the
// number, a dot, the label. Verified 2026-09-14 against Claude Code
// 2.1.263: permission prompts list "1. Yes" / "2. Yes, and …" / "N. No",
// AskUserQuestion lists its options the same way, and the digit alone
// selects.
var optionLine = regexp.MustCompile(`^\s*(?:[❯>]\s*)?([1-9])\.\s+(.+?)\s*$`)

// optionsOnScreen returns the prompt's choices: the last run of lines on
// screen numbered 1, 2, 3… in order, other lines between them (an option's
// description) allowed. The prompt sits at the bottom, so the last run is
// the live one; a numbered list in the agent's own prose above it is
// superseded by the prompt's own "1.".
func optionsOnScreen(screen string) []option {
	var run []option
	for _, line := range strings.Split(screen, "\n") {
		m := optionLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch {
		case m[1] == "1":
			run = []option{{key: "1", label: m[2]}}
		case len(run) > 0 && m[1] == string(rune('1'+len(run))):
			run = append(run, option{key: m[1], label: m[2]})
		}
	}
	return run
}

// A picker picks the key to type for the prompt's options; false means
// the prompt on screen is not the one the reply was for.
type picker func(opts []option) (key string, ok bool)

// chooserFor resolves a reply's params for it into the option to type.
func chooserFor(it *item, params map[string]any) (picker, error) {
	switch it.kind {
	case wire.FeedKindPermissionRequest:
		mode, _ := params["mode"].(string)
		return permissionChooser(mode)
	case wire.FeedKindQuestion:
		return questionChooser(it, params)
	}
	return nil, fmt.Errorf("feed kind %q: %w", it.kind, host.ErrUnsupported)
}

// permissionChooser maps a reply mode to the prompt's option text. The
// prompt has no "all tools" or "bypass" choice, so those YOLO modes take
// the per-prompt "always" option, and a prompt without one (Write offers
// only "switch to accept edits") is still approved this once. Deny is the
// "No" option, or Esc when the prompt has none.
func permissionChooser(mode string) (picker, error) {
	switch mode {
	case "once":
		return func(opts []option) (string, bool) { return keyFor(opts, isYes) }, nil
	case "always", "all", "bypass":
		return func(opts []option) (string, bool) {
			if key, ok := keyFor(opts, isAlways); ok {
				return key, true
			}
			return keyFor(opts, isYes)
		}, nil
	case "deny":
		return func(opts []option) (string, bool) {
			if key, ok := keyFor(opts, isNo); ok {
				return key, true
			}
			return "Escape", len(opts) > 0
		}, nil
	}
	return nil, fmt.Errorf("permission mode %q: %w", mode, host.ErrUnsupported)
}

func isYes(label string) bool { return label == "Yes" }
func isNo(label string) bool  { return label == "No" }

// isAlways is the option that stops the prompt recurring for this tool;
// "switch to auto mode" is excluded because it drops every future prompt,
// which is not what always-allow-this means.
func isAlways(label string) bool {
	return strings.HasPrefix(label, "Yes, and ") && !strings.Contains(label, "auto mode")
}

func keyFor(opts []option, match func(label string) bool) (string, bool) {
	for _, o := range opts {
		if match(o.label) {
			return o.key, true
		}
	}
	return "", false
}

// questionChooser types the option whose label the phone selected. Only a
// single-select prompt with one question is answered this way: a digit
// selects and submits at once, so a multi-select or a second question would
// need keystrokes the probe never saw.
func questionChooser(it *item, params map[string]any) (picker, error) {
	qs := questionsOf(it.toolInput)
	if len(qs) != 1 || qs[0].MultiSelect {
		return nil, fmt.Errorf("multi-select or multi-question prompt: %w", host.ErrUnsupported)
	}
	raw, _ := params["selections"].([]any)
	if len(raw) != 1 {
		return nil, fmt.Errorf("exactly one selection expected: %w", host.ErrUnsupported)
	}
	want, _ := raw[0].(string)
	if want == "" {
		return nil, fmt.Errorf("empty selection: %w", host.ErrUnsupported)
	}
	return func(opts []option) (string, bool) {
		return keyFor(opts, func(label string) bool { return label == want })
	}, nil
}
