package auth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func protected(t *testing.T) (*Store, http.Handler, string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	tenant, _ := s.CreateTenant()
	tok, _ := s.Issue(tenant, "phone", testPubkey)
	h := Require(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dev, ok := DeviceFromContext(r.Context())
		if !ok || dev.Name != "phone" {
			t.Errorf("handler did not see device in context: %+v ok=%v", dev, ok)
		}
		w.WriteHeader(http.StatusOK)
	}))
	return s, h, tok
}

func TestRequireNoHeader401(t *testing.T) {
	_, h, _ := protected(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestRequireBadToken401(t *testing.T) {
	_, h, _ := protected(t)
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestRequireGoodToken200(t *testing.T) {
	_, h, tok := protected(t)
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// captureLog redirects the default slog logger to a buffer for the duration
// of the test, restoring it on cleanup.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func reject(t *testing.T, h http.Handler, token string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/sessions", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
}

// A rejection used to leave no trace at all, which is why the 2026-08-21
// lockout had to be diagnosed by reading SQLite by hand (cmux-app-hr1). The
// prefix is what makes the line actionable: it is the same value
// `term-bridge devices list` prints.
func TestRequireLogsRejectionWithHashPrefixNotTheToken(t *testing.T) {
	_, h, _ := protected(t)
	buf := captureLog(t)

	reject(t, h, "wrong")

	logged := buf.String()
	wantPrefix := hashToken("wrong")[:rejectionHashLen]
	if !strings.Contains(logged, wantPrefix) {
		t.Fatalf("want the token hash prefix %q in the log, got: %s", wantPrefix, logged)
	}
	if strings.Contains(logged, "wrong") {
		t.Fatalf("the raw token must never be logged, got: %s", logged)
	}
	if strings.Contains(logged, hashToken("wrong")) {
		t.Fatalf("only a prefix of the hash may be logged, got: %s", logged)
	}
	if !strings.Contains(logged, "/sessions") {
		t.Fatalf("want the route in the log, got: %s", logged)
	}
}

// A rejected client retries -- that is normal, not an attack -- and unbounded
// per-attempt logging is what already made the agent log 84%% reconnect noise
// (cmux-app-5v1).
func TestRequireRateLimitsRepeatedRejections(t *testing.T) {
	_, h, _ := protected(t)
	buf := captureLog(t)

	for range 5 {
		reject(t, h, "wrong")
	}

	if got := strings.Count(buf.String(), "rejected unknown device"); got != 1 {
		t.Fatalf("want 1 rejection line for a repeating client, got %d: %s", got, buf.String())
	}
}

func TestRequireLogsDistinctTokensSeparately(t *testing.T) {
	_, h, _ := protected(t)
	buf := captureLog(t)

	reject(t, h, "wrong-one")
	reject(t, h, "wrong-two")

	if got := strings.Count(buf.String(), "rejected unknown device"); got != 2 {
		t.Fatalf("want one line per distinct token, got %d: %s", got, buf.String())
	}
}

// hashToken("") is a fixed value that looks exactly like a real device hash,
// so an anonymous probe must not be reported as one.
func TestRequireLogsMissingTokenWithoutInventingAHash(t *testing.T) {
	_, h, _ := protected(t)
	buf := captureLog(t)

	reject(t, h, "")

	logged := buf.String()
	if !strings.Contains(logged, anonymousRejection) {
		t.Fatalf("want %q for a request with no bearer token, got: %s", anonymousRejection, logged)
	}
	if strings.Contains(logged, hashToken("")[:rejectionHashLen]) {
		t.Fatalf("an absent token must not be reported as a device hash, got: %s", logged)
	}
}

// An infrastructure error must never be reported as an authentication
// failure, in the log any more than in the status code.
func TestRequireStoreFailureStillLogsAsAnErrorAnd500s(t *testing.T) {
	s, h, _ := protected(t)
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	buf := captureLog(t)

	req := httptest.NewRequest("GET", "/sessions", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rr.Code)
	}
	logged := buf.String()
	if !strings.Contains(logged, "auth: verify") {
		t.Fatalf("want the existing store-failure line, got: %s", logged)
	}
	if strings.Contains(logged, "rejected unknown device") {
		t.Fatalf("a store failure must not be logged as a rejected device, got: %s", logged)
	}
}
