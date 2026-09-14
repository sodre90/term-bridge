package server

import (
	"net/http"

	"github.com/sodre90/term-bridge/internal/httpjson"
	"github.com/sodre90/term-bridge/internal/version"
	"github.com/sodre90/term-bridge/internal/wire"
)

// handleVersion answers with the running agent's version.
//
// It sits inside the authenticated route set rather than beside a health
// check: a build number tells an unauthenticated caller which fixes this agent
// is missing, and nothing needs it before pairing.
//
// Deliberately not folded into GET /sessions, which the app polls: the version
// changes only when the process restarts, and putting it on the hot path would
// pay for it on every refresh to answer a question asked once per visit to the
// Connections screen.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	httpjson.Write(w, http.StatusOK, wire.VersionResponse{Bridge: version.String()})
}
