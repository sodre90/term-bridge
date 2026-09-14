package tmuxhost

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Ids on the wire are `tmux-<epoch>-w<N>` for window @N and
// `tmux-<epoch>-p<N>` for pane %N, where epoch is the tmux server's
// #{start_time}. tmux reuses @N/%N after a server restart, and a restart
// also kills every process, so the epoch makes an id from a dead server
// unmatchable instead of silently naming a new window. Only [A-Za-z0-9-]:
// tmux's own @ and % sigils would be malformed percent-escapes in a path.

const idPrefix = "tmux-"

var idPattern = regexp.MustCompile(`^tmux-([0-9]+)-([wp])([0-9]+)$`)

type idKind byte

const (
	windowID idKind = 'w'
	paneID   idKind = 'p'
)

type id struct {
	epoch int64
	kind  idKind
	n     int
}

func (i id) String() string {
	return fmt.Sprintf("%s%d-%c%d", idPrefix, i.epoch, i.kind, i.n)
}

// target is the id in tmux's own spelling, for -t.
func (i id) target() string {
	if i.kind == windowID {
		return "@" + strconv.Itoa(i.n)
	}
	return "%" + strconv.Itoa(i.n)
}

func encodeID(epoch int64, tmuxID string) string {
	kind := paneID
	if strings.HasPrefix(tmuxID, "@") {
		kind = windowID
	}
	n, _ := strconv.Atoi(tmuxID[1:])
	return id{epoch: epoch, kind: kind, n: n}.String()
}

func decodeID(s string) (id, bool) {
	m := idPattern.FindStringSubmatch(s)
	if m == nil {
		return id{}, false
	}
	epoch, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return id{}, false
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return id{}, false
	}
	return id{epoch: epoch, kind: idKind(m[2][0]), n: n}, true
}

// validID is the shape check the server runs on every path id.
func validID(s string) bool { return idPattern.MatchString(s) }
