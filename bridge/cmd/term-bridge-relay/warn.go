package main

import (
	"log/slog"

	"github.com/sodre90/term-bridge/internal/wire"
)

// warnFCMClientConfig says out loud what a phone can never tell the operator:
// that the Firebase config it would have been handed at pairing is not going
// to be sent. Both cases are otherwise silent -- the pairing response simply
// omits the block -- and the second is actively misleading, because the relay
// still logs that push is enabled while every phone paired against it goes on
// receiving nothing.
func warnFCMClientConfig(c wire.FCMClientConfig, canSend bool) {
	switch {
	case c.PartiallyConfigured():
		slog.Warn("fcm client config incomplete -- withholding it from pairing; phones will not receive push",
			"have_project_id", c.ProjectID != "", "have_app_id", c.AppID != "",
			"have_api_key", c.APIKey != "", "have_sender_id", c.SenderID != "")
	case canSend && !c.Configured():
		slog.Warn("fcm credentials are set but the client config is not -- only phones with a google-services.json compiled in will receive push; set fcm_app_id, fcm_api_key and fcm_sender_id")
	}
}
