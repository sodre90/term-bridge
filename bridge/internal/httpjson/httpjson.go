// Package httpjson is the bridge's single JSON response writer. Every prod
// handler across term-bridge-relay and term-bridge routes both its success and
// error bodies through Write/Error, so an error response is always
// {"error":"..."} with the matching status code -- one shape for the app to
// parse instead of each package rolling its own near-identical writer (or,
// worse, a plain-text http.Error body on some routes and JSON on others).
package httpjson

import (
	"encoding/json"
	"net/http"
)

// Write encodes body as JSON and writes it with the given status code.
func Write(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// Error writes {"error": msg} with the given status code.
func Error(w http.ResponseWriter, code int, msg string) {
	Write(w, code, map[string]string{"error": msg})
}
