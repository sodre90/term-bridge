package main

import (
	"testing"

	"github.com/sodre90/term-bridge/internal/config"
	"github.com/sodre90/term-bridge/internal/wire"
)

// Four same-typed strings: transposing any two compiles cleanly and every
// behavioural test still passes, because nothing downstream can tell an app id
// from an api key. Only a mapping assertion catches it, and a phone handed a
// transposed pair fails to initialise Firebase with no useful error.
func TestFCMClientConfigMapsEachFieldToItsOwnSetting(t *testing.T) {
	got := fcmClientConfig(config.AgentConfig{
		FCMProjectID: "project-id-value",
		FCMAppID:     "app-id-value",
		FCMAPIKey:    "api-key-value",
		FCMSenderID:  "sender-id-value",
	})
	want := wire.FCMClientConfig{
		ProjectID: "project-id-value",
		AppID:     "app-id-value",
		APIKey:    "api-key-value",
		SenderID:  "sender-id-value",
	}
	if got != want {
		t.Fatalf("field mapping = %+v, want %+v", got, want)
	}
}

// An agent with no push settings must produce a config that reports itself
// unconfigured, so the pairing response omits the block entirely.
func TestFCMClientConfigOfEmptyAgentConfigIsUnconfigured(t *testing.T) {
	if got := fcmClientConfig(config.AgentConfig{}); got.Configured() {
		t.Fatalf("empty agent config produced a configured value: %+v", got)
	}
}
