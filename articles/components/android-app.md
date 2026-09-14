---
type: article
description: "Android app component reference: requirements, build, pairing setup (one or more hosts), push, known gaps, and out-of-scope items."
status: canonical
authored: 2026-07-21
author: sodre90
tags:
  - article
  - canonical
  - android
  - component
---
## Summary

The Android app (`com.sodre90.cmuxremote`) is a native Kotlin/Compose client for `term-bridge`. It connects to the bridge behind the mTLS nginx edge to list the sessions on your hosts (cmux workspaces on a Mac, tmux windows on a Linux box), drive a live terminal, and answer agent prompts, with optional FCM push. It pairs with any number of hosts, keeps credentials, keys and workspace order per host (keyed by the agent's identity key), and switches between them from the sessions title. It depends only on the bridge's documented HTTP/WebSocket contract, never on cmux or tmux internals; the `host` block on `GET /sessions` tells it what each host can do (a tmux host has no tabs and, until its hooks land, no Inbox/YOLO).

## Body

### Trust model

Every request carries an `Authorization: Bearer <device-token>` minted at pairing — no client TLS certificate on the device side (only agents have one). Once paired, request/response bodies and terminal frames are also end-to-end encrypted between the phone and that host's agent (X25519 ECDH + HKDF derived during pairing), so the relay can route traffic but not read it. See [pairing-e2e-encryption](../features/pairing-e2e-encryption.md).

### Requirements

- Android Studio (Ladybug or newer) with the Android SDK (compileSdk 35).
- JDK 17 (the Gradle toolchain targets 17).
- A running `term-bridge` reachable through the nginx mTLS edge — see [bridge](./bridge.md). The phone must be able to reach that DNS name (over the internet or LAN).

### Build & run

```bash
cd android
./gradlew :app:assembleDebug        # build the debug APK
./gradlew :app:testDebugUnitTest    # run the JVM unit tests
```

Or open `android/` in Android Studio, sync, then Run `app` on a device or emulator.

### First-run setup (Pairing screen)

On first launch — or any time the app has no bridge config yet — it opens the Pairing screen instead of the sessions list. Pairing is entirely self-service: nothing to generate or paste by hand.

On the host, with its agent running:

```bash
term-bridge pair-device --config ~/.config/term-bridge/agent.toml
```

This prints a QR code and, alongside it, a short code for manual entry.

**Option 1: scan the QR code.** Grant the camera permission, point it at the QR code. The QR payload carries the server URL, a one-time pairing code, and the agent's public key — the app redeems the code with the relay, generates its own X25519 keypair (kept in `EncryptedSharedPreferences`, persisted thereafter), derives a shared secret with the agent, and stores the bridge's base URL and the bearer token it's issued.

**Option 2: enter the server URL and code manually.** No camera handy, or pairing remotely (e.g. over SSH into the host)? Tap "Enter server URL and code manually" and fill in the same `https://` base the QR's `pair_url` uses, plus the printed pairing code. The app resolves the agent's public key via `GET /devices/pair-info/{code}` and completes the same handshake as the QR path.

Either way, once pairing succeeds the app moves to the sessions list; the start screen on subsequent launches is the sessions list whenever a bridge config is already present. A pairing code is single-use and expires (10 minutes).

### Push notifications (optional)

Push is off by default and the app builds and runs without any Firebase config.

1. Create a Firebase project and add an Android app with applicationId `com.sodre90.cmuxremote`.
2. Configure the bridge with the client half of the config (`fcm_app_id`, `fcm_api_key`, `fcm_sender_id`); it reaches the app at pairing, so no `google-services.json` need be compiled in. Placing one at `android/app/google-services.json` still works and takes precedence. The Gradle build applies the `com.google.gms.google-services` plugin only when that file exists.
3. Configure the bridge's FCM sender (`fcm_project_id` + `fcm_credentials`) — see [bridge](./bridge.md) and [push-notifications](../features/push-notifications.md).

The app registers its FCM token via `POST /devices/register` on launch and on token rotation. When the bridge sends a `type=attention` data message, the app posts a high-priority notification that deep-links into the exact workspace that needs attention.

### Known gaps / assumptions

- **Feed reply `request_id`.** The bridge reads `request_id` from the reply body. cmux's exact reply param names are unconfirmed, so the inbox currently sends the feed id as `request_id` and params `{"decision":"approve"|"deny"}` for permission/exit-plan prompts and `{"answer": <text>}` for questions. Not confirmed against a live cmux prompt at the time this was written.

Two earlier open questions are resolved: workspace id and terminal surface id are deliberately distinct (see the workspace→panes model in `GET /sessions`'s `terminals[]`, and [android-terminal-foundation](../features/android-terminal-foundation.md) for the surface-id design rationale), and render-grid colors are confirmed `#rrggbb`/`#aarrggbb` hex strings, verified live.

### Out of scope (for now)

File-diff viewing, a merged sessions list across hosts, biometric lock, and tablet-specific layouts. The bridge performs read methods, terminal input/replay, feed replies, workspace rename, and workspace/pane create, select and close (confirmed on the phone where destructive) — it never restores workspaces/terminals.

## References

- [android/README.md](../../android/README.md)
