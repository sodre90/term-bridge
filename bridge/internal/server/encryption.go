package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sodre90/term-bridge/internal/e2e"
	"github.com/sodre90/term-bridge/internal/httpjson"
	"github.com/sodre90/term-bridge/internal/metrics"
)

// SetSessions enables the opt-in e2e content-encryption layer. Called only by
// runAgent's production wiring (Task 14); no test calls this, so every
// pre-existing test continues to exercise the plaintext code path unchanged.
func (s *Server) SetSessions(sessions *e2e.Store) { s.sessions = sessions }

// encryptionMiddleware transparently decrypts an e2e-enveloped request body
// and encrypts the response body, keyed by the X-Device-ID header the
// relay's proxy Director injects (internal/relay/proxy.go). A request with
// no X-Device-ID, or one the session store doesn't recognize, is rejected
// before reaching the wrapped handler — once encryption is enabled there is
// no plaintext fallback (see the Global Constraint on full enforcement).
// WebSocket upgrade requests (/terminal/{id}, /events) pass through
// untouched: they hijack the connection, so a generic body wrapper can't
// reach them, and they carry their own frame-level encryption (Tasks 12-13).
func (s *Server) encryptionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.sessions == nil || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			next.ServeHTTP(w, r)
			return
		}
		deviceID := r.Header.Get("X-Device-ID")
		if deviceID == "" {
			// Shouldn't happen given the proxy always sets it, but defensively:
			// per the spec's error-handling section, a missing header gets the
			// generic "unknown_device" 401, distinct from the 409 below for a
			// present-but-unrecognized device (e.g. this agent's local e2e
			// state was wiped) -- the two point the app at different recovery
			// UX (re-check auth vs. re-pair).
			httpjson.Error(w, http.StatusUnauthorized, "unknown_device")
			return
		}
		if _, ok := s.sessions.SharedSecret(deviceID); !ok {
			httpjson.Error(w, http.StatusConflict, "not_paired")
			return
		}

		if r.ContentLength != 0 && r.Body != nil {
			// Every plaintext handler behind this middleware caps its own
			// body at 4KB (see rename.go/feed.go/yolo.go/push.go); the e2e
			// envelope adds base64 + JSON + AEAD-tag overhead on top of
			// that, so 8KB bounds the envelope comfortably without letting
			// a paired-but-malicious device OOM the agent with an
			// arbitrarily large body.
			envelope, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
			if err != nil {
				httpjson.Error(w, http.StatusBadRequest, "read_failed")
				return
			}
			if len(envelope) > 0 {
				plaintext, err := s.sessions.DecryptBody(deviceID, envelope)
				if err != nil {
					slog.Warn("server: decrypt body failed", "route", r.URL.Path, "device", deviceLogID(deviceID), "err", err)
					metrics.E2EDecryptFailuresTotal.Add("body", 1)
					httpjson.Error(w, http.StatusBadRequest, "decrypt_failed")
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(plaintext))
				r.ContentLength = int64(len(plaintext))
			}
		}

		rec := &encryptingResponseWriter{ResponseWriter: w, deviceID: deviceID, sessions: s.sessions}
		next.ServeHTTP(rec, r)
		rec.flush()
	})
}

// encryptingResponseWriter buffers a handler's plaintext response and
// encrypts it as a single e2e envelope on flush, rather than encrypting each
// Write call separately — every handler behind this middleware writes one
// JSON body in one call, so this keeps envelope-counter usage at exactly one
// increment per response.
type encryptingResponseWriter struct {
	http.ResponseWriter
	deviceID string
	sessions *e2e.Store
	buf      bytes.Buffer
	status   int
}

func (w *encryptingResponseWriter) WriteHeader(status int) { w.status = status }

func (w *encryptingResponseWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

func (w *encryptingResponseWriter) flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	envelope, err := w.sessions.EncryptBody(w.deviceID, w.buf.Bytes())
	if err != nil {
		httpjson.Error(w.ResponseWriter, http.StatusInternalServerError, "encrypt_failed")
		return
	}
	// This is the sole point a body is encrypted; the marker is set here and
	// only here so the client decrypts iff the marker is present.
	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	w.ResponseWriter.Header().Set("X-Cmux-Encrypted", "1")
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(envelope)
}
