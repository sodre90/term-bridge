package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sodre90/term-bridge/internal/httpjson"
)

type ctxKey int

const deviceKey ctxKey = 0

// Require wraps next so that only requests bearing a valid device token reach
// it. The token is read from the "Authorization: Bearer <token>" header. A
// token that verifies against no device gets a 401; a genuine store failure
// (the DB itself misbehaving) gets a 500 instead -- an infra error must never
// masquerade as an authentication failure. On success the resolved Device is
// attached to the request context (see DeviceFromContext).
func Require(s *Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := BearerToken(r)
		dev, err := s.Verify(tok)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				s.logRejection(r, tok)
				httpjson.Error(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			slog.Error("auth: verify", "err", err)
			httpjson.Error(w, http.StatusInternalServerError, "internal_error")
			return
		}
		ctx := context.WithValue(r.Context(), deviceKey, dev)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// rejectionHashLen matches the prefix width `term-bridge devices list` and
// `term-bridge-relay devices list` print, so a rejection line can be matched against
// a listing row without converting anything. Kept in step with those by hand:
// their own constant lives in package main and cannot be imported here.
const rejectionHashLen = 12

// anonymousRejection stands in for the token hash when there was no bearer
// token at all. Never hashToken(""), which is a fixed value that looks
// exactly like a real device hash and would send an operator hunting for a
// device that never existed.
const anonymousRejection = "missing"

// logRejection records that a device was turned away, which nothing used to
// report at all: diagnosing the 2026-08-21 lockout meant reading SQLite by
// hand and correlating pairing_codes against reaper log lines (cmux-app-hr1).
//
// Only a prefix of the token's hash is logged -- never the token, never the
// full hash (README's "never log secrets"). Rate limited per prefix, because
// a rejected client retrying is the normal case and must not be able to flood
// the log; cmux-app-5v1 is a live example of what unbounded per-attempt
// logging already costs this file.
func (s *Store) logRejection(r *http.Request, token string) {
	hashPrefix := anonymousRejection
	if token != "" {
		hashPrefix = hashToken(token)[:rejectionHashLen]
	}
	if !s.rejectionLog.Allow(hashPrefix) {
		return
	}
	slog.Warn("auth: rejected unknown device", "route", r.URL.Path, "device", hashPrefix)
}

// DeviceFromContext returns the Device attached by Require, if any.
func DeviceFromContext(ctx context.Context) (Device, bool) {
	d, ok := ctx.Value(deviceKey).(Device)
	return d, ok
}

// BearerToken extracts the raw "Authorization: Bearer <token>" value from a
// request, or "" if absent/malformed. Exported so handlers that already went
// through Require (and so already have a Device in context) can recover the
// original raw token for calls that key off it directly, like SetFCMToken.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
