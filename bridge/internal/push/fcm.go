// Package push sends agent-attention notifications via the Firebase Cloud
// Messaging HTTP v1 API. The HTTP endpoint and OAuth token are injectable so the
// sender can be unit-tested without contacting Google.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/sodre90/term-bridge/internal/metrics"
)

// ErrTokenDead marks a send that failed because the registration token will
// never work again -- the app was uninstalled, its data cleared, the token
// rotated, or it expired after FCM's 270 days of inactivity. Callers should
// drop the stored token rather than retry it.
//
// This is the answer to "how do you tell a dead token from a live one", and
// the answer is not what cmux-app-6u7 assumed: FCM HTTP v1 does NOT return
// 2xx for a decommissioned token. It returns 404 with errorCode UNREGISTERED
// (https://firebase.google.com/docs/cloud-messaging/manage-tokens). What was
// missing was not the signal but any use of it -- "fcm status 404" read
// exactly like the transient "fcm status 503" next to it in the log, so
// nothing could act on either.
var ErrTokenDead = errors.New("fcm registration token is no longer registered")

// How much of an error body to read. Enough for the JSON status and error
// details, bounded so a misbehaving endpoint cannot make the agent read an
// unbounded response.
const maxErrorBody = 8 << 10

// fcmErrorBody is the subset of Google's standard API error shape that says
// why a send was rejected.
type fcmErrorBody struct {
	Error struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Details []struct {
			Type      string `json:"@type"`
			ErrorCode string `json:"errorCode"`
		} `json:"details"`
	} `json:"error"`
}

// classifyError turns a non-2xx send response into an error that says
// whether the token is worth keeping.
//
// Only UNREGISTERED is treated as fatal to the token. FCM also returns 400
// INVALID_ARGUMENT for a dead registration, but it returns the same thing for
// a malformed message -- and this sender builds every message itself, so a
// bug here would look identical to every token in the store dying at once.
// Dropping them all on our own payload error is a far worse failure than
// keeping a token that no longer works, so INVALID_ARGUMENT is reported as an
// ordinary failure, distinctly enough to grep for.
func classifyError(status int, body []byte) error {
	var parsed fcmErrorBody
	_ = json.Unmarshal(body, &parsed)
	code := parsed.Error.Status
	for _, d := range parsed.Error.Details {
		if d.ErrorCode != "" {
			code = d.ErrorCode
		}
	}
	if code == "" {
		return fmt.Errorf("fcm status %d", status)
	}
	if status == http.StatusNotFound && code == "UNREGISTERED" {
		return fmt.Errorf("fcm status %d (%s): %w", status, code, ErrTokenDead)
	}
	return fmt.Errorf("fcm status %d (%s)", status, code)
}

// Sender posts FCM HTTP v1 messages.
type Sender struct {
	ProjectID string
	// BaseURL defaults to https://fcm.googleapis.com. Override in tests.
	BaseURL string
	// HTTP defaults to http.DefaultClient.
	HTTP *http.Client
	// Token returns a bearer OAuth token with the firebase.messaging scope.
	Token func(context.Context) (string, error)
}

// Send delivers a high-priority data message to one FCM registration token.
// title and body are carried inside the data payload so the app controls
// presentation.
func (s *Sender) Send(ctx context.Context, fcmToken, title, body string, data map[string]string) error {
	d := map[string]string{}
	for k, v := range data {
		d[k] = v
	}
	d["title"] = title
	d["body"] = body

	payload := map[string]any{
		"message": map[string]any{
			"token":   fcmToken,
			"data":    d,
			"android": map[string]any{"priority": "high"},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	base := s.BaseURL
	if base == "" {
		base = "https://fcm.googleapis.com"
	}
	url := base + "/v1/projects/" + s.ProjectID + "/messages:send"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	tok, err := s.Token(ctx)
	if err != nil {
		return fmt.Errorf("fcm oauth: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return classifyError(resp.StatusCode, errBody)
	}
	return nil
}

// TokenStore is the part of auth.Store that DropDeadToken needs. An interface
// so this package stays free of a dependency on auth, which the sender
// otherwise has no reason to know about.
type TokenStore interface {
	ClearFCMToken(fcm string) (int, error)
}

// DropDeadToken clears fcmToken from store when err reports it as no longer
// registered, and says whether it did. Both fan-out paths -- the agent's own
// direct-mode push and the relay's -- call it, because a token can be dead in
// either store and neither can see the other's.
//
// The token itself is never logged: it is the routing credential for a
// device's notifications.
func DropDeadToken(store TokenStore, fcmToken string, err error) bool {
	if store == nil || !errors.Is(err, ErrTokenDead) {
		return false
	}
	rows, clearErr := store.ClearFCMToken(fcmToken)
	if clearErr != nil {
		slog.Error("push: fcm reported a token unregistered but it could not be dropped", "err", clearErr)
		return false
	}
	metrics.PushTokensDroppedTotal.Add(1)
	slog.Warn("push: dropped an fcm token fcm reports as unregistered -- the device re-registers on its next app launch",
		"device_rows", rows)
	return true
}
