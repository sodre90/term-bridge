package relay

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/backoff"
	"github.com/sodre90/term-bridge/internal/metrics"
	pushpkg "github.com/sodre90/term-bridge/internal/push"
	"github.com/sodre90/term-bridge/internal/wire"
)

// Pusher delivers an attention push to a single device token. push.Sender
// satisfies it.
type Pusher interface {
	Send(ctx context.Context, fcmToken, title, body string, data map[string]string) error
}

// MonitorAgent subscribes to the agent's /events over the tunnel and fans
// blocking prompts out to FCM, scoped to tenantID's own devices only. It
// returns when ctx is cancelled or the session dies. relayToken authenticates
// to the agent's trusted handler.
func MonitorAgent(ctx context.Context, tenantID string, sess *yamux.Session, relayToken string, store *auth.Store, push Pusher) {
	if push == nil {
		return
	}
	retry := backoff.New(time.Second, 30*time.Second)
	for ctx.Err() == nil {
		if err := subscribeOnce(tenantID, sess, relayToken, store, push); err != nil && sess.IsClosed() {
			return
		}
		if !backoff.Sleep(ctx, retry.Next()) {
			return
		}
	}
}

func subscribeOnce(tenantID string, sess *yamux.Session, relayToken string, store *auth.Store, push Pusher) error {
	d := websocket.Dialer{
		NetDial: func(_, _ string) (net.Conn, error) { return sess.Open() },
	}
	ws, _, err := d.Dial("ws://agent/events", http.Header{"X-Relay-Token": {relayToken}})
	if err != nil {
		return err
	}
	defer func() { _ = ws.Close() }()
	for {
		var f wire.EventFrame
		if err := ws.ReadJSON(&f); err != nil {
			return err
		}
		if f.NeedsAttention {
			fanout(tenantID, store, push, f)
		}
	}
}

// fanout sends one push per registered device, each carrying only that
// device's own e2e-encrypted payload from f.EncryptedPush (built agent-side
// by buildEncryptedPush) -- the relay never has the shared secrets needed to
// read or build notification content itself (see events.go's
// writeEventFrame, which redacts Title/Preview before this subscription ever
// sees the frame). A device missing from EncryptedPush still gets a push
// with routing metadata only, so the app can show a generic notification
// rather than nothing.
func fanout(tenantID string, store *auth.Store, push Pusher, f wire.EventFrame) {
	devices := store.TenantFCMDevices(tenantID)
	if len(devices) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sent, failed, encrypted, dropped := 0, 0, 0, 0
	for _, dev := range devices {
		data := map[string]string{
			"type":         "attention",
			"feed_id":      f.FeedID,
			"workspace_id": f.WorkspaceID,
			"surface_id":   f.SurfaceID,
			"kind":         f.Kind,
			"slot":         "relay",
		}
		if blob, ok := f.EncryptedPush[dev.DeviceID]; ok {
			data["e2e"] = blob
			encrypted++
		}
		if err := push.Send(ctx, dev.FCMToken, "", "", data); err != nil {
			failed++
			if pushpkg.DropDeadToken(store, dev.FCMToken, err) {
				dropped++
			}
			slog.Warn("relay: attention push failed", "tenant_id", tenantID, "kind", f.Kind, "workspace_id", f.WorkspaceID, "err", err)
			continue
		}
		sent++
	}
	metrics.PushSentTotal.Add(int64(sent))
	metrics.PushFailedTotal.Add(int64(failed))
	slog.Info("relay: attention push sent", "tenant_id", tenantID, "kind", f.Kind, "workspace_id", f.WorkspaceID, "sent", sent, "failed", failed, "dropped", dropped, "encrypted", encrypted)
}
