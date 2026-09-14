# Term Bridge (Android)

A native Android client for [`term-bridge`](../bridge). It connects to the bridge
**behind your mTLS nginx edge** to list the sessions on your hosts -- cmux
workspaces on a Mac, tmux windows on a Linux box -- drive a live terminal, and
answer agent prompts — with optional FCM push when an agent needs your
attention. Pair as many hosts as you like and switch between them from the
sessions title.

The app depends only on the bridge's documented HTTP/WebSocket contract
(`GET /sessions`, `WS /events`, `WS /terminal/{id}`, `POST /feed/{id}/reply`,
`POST /devices/register`), never on cmux or tmux internals; `GET /sessions`
also carries a `host` block (name, kind, capabilities) that the app gates its
UI on -- a tmux host has no tabs and, until its hooks land, no Inbox/YOLO.
Every request carries an `Authorization: Bearer <device-token>` minted at
pairing — no client TLS certificate on the device side (only agents have
one). Once paired, request/response bodies and terminal frames are also
end-to-end encrypted between the phone and that host's agent (X25519 ECDH +
HKDF derived during pairing), so the relay can route traffic but not read
it. Credentials, keys and workspace order are kept per host, keyed by the
agent's identity key, so the same machine's relay and Tailscale pairings
share one host entry; display preferences (font zoom, wheel scrolling, poll
intervals) stay phone-wide.

## Requirements

- Android Studio (Narwhal 2025.1 or newer — the project builds with AGP 8.13)
  with the Android SDK (compileSdk 36, minSdk 26).
- JDK 21+. App code targets JVM 17, but a test-only dependency
  (`lazysodium-java`) ships Java 21 class files, so `testDebugUnitTest` needs a
  21+ runtime.
- A running `term-bridge` reachable through your nginx mTLS edge — see
  [`bridge/README.md`](../bridge/README.md). The phone must be able to reach that
  DNS name (over the internet or your LAN).

## Build & run

```bash
# from the repo root
cd android
./gradlew :app:assembleDebug        # build the debug APK
./gradlew :app:testDebugUnitTest    # run the JVM unit tests
```

Or open the `android/` directory in Android Studio, let it sync, then Run `app`
on a device or emulator.

## First-run setup (Pairing screen)

On first launch — or any time the app has no bridge config yet — it opens the
**Pairing** screen instead of the sessions list. Pairing is entirely
self-service now: nothing to generate or paste by hand.

On the host, with its agent running (see [`bridge/README.md` → Agent
(Mac)](../bridge/README.md#agent-mac) or [→ Agent (Linux,
tmux)](../bridge/README.md#agent-linux-tmux)):

```bash
term-bridge pair-device --config ~/.config/term-bridge/agent.toml
```

This prints a QR code and, alongside it, a short code for manual entry.

### Option 1: scan the QR code

Grant the camera permission when prompted, then point it at the QR code
printed above. The QR payload carries the server URL, a one-time pairing
code, and the agent's public key — the app redeems the code with the relay,
generates its own X25519 keypair (kept in `EncryptedSharedPreferences`,
persisted thereafter), derives a shared secret with the agent, and stores the
bridge's base URL and the bearer token it's issued. No further input needed.

### Option 2: enter the server URL and code manually

No camera handy, or pairing remotely (e.g. over SSH into the host)? Tap
**"Enter server URL and code manually"** and fill in:

- **Server URL** — the same `https://` base the QR's `pair_url` uses (e.g.
  `https://term-bridge.example.com`).
- **Pairing code** — the short code `pair-device` printed next to the QR.

The app resolves the agent's public key via the relay's `GET
/devices/pair-info/{code}` and completes the exact same handshake as the QR
path from there.

### Confirm the fingerprint (both paths)

Before either path completes, the phone shows a short fingerprint and waits
for you to confirm it, while `pair-device` prints the same fingerprint on the
host and asks `Confirm? [y/N]:`. **Compare the two and only accept if they
match** — this is what stops the relay from swapping in its own key and
reading your traffic. Anything other than `y` on the host aborts the pairing.

Either way, once pairing succeeds the app moves to the sessions list; the
start screen on subsequent launches is the sessions list whenever a bridge
config is already present. A pairing code is single-use and expires (10
minutes) — if it's stale, generate a fresh one with `pair-device` and retry.

### More hosts

Tap the host name in the sessions title → **Pair another host…** (or
Settings → Connections → Pair another host) and repeat the steps above on
the second machine. The new host is selected as soon as it pairs; its name
and kind (`cmux` / `tmux`) are learned from the host on the first
`GET /sessions`. The same menu switches hosts; Connections shows one card
per host with its Relay and Tailscale slots, and forgetting a host's last
slot removes the host.

### Direct (Tailscale) mode and dual pairing

Each host on the Connections screen holds two independent slots — a relay pairing and a
direct (Tailscale) pairing — and you can fill both. When both are paired the
app tries the relay first and transparently fails over to direct, so pairing
both is the recommended setup. See
[`bridge/README.md`](../bridge/README.md) for the agent side.

## Push notifications (optional)

Push is **off by default and the app builds and runs without any Firebase
config.** The Firebase client config travels from the bridge to the app on the
pairing response, so a prebuilt APK can turn push on with no Android toolchain
-- nothing is compiled in.

1. Create a Firebase project and add an Android app with applicationId
   `com.sodre90.cmuxremote`.
2. Configure the bridge with both halves of the Firebase config: the sender
   half (`fcm_project_id` + `fcm_credentials`) and the client half
   (`fcm_app_id`, `fcm_api_key`, `fcm_sender_id`). See
   [`bridge/README.md`](../bridge/README.md).
3. Pair (or re-pair) the phone. The config is delivered by pairing and only by
   pairing, so a phone paired before the bridge was configured must pair again.

All four client fields must be set on the bridge or none is sent: Firebase
rejects partial options, so a half-filled config is treated as absent and the
bridge logs a warning at startup.

A build that does ship `android/app/google-services.json` still works and takes
precedence -- the Gradle plugin applies only when that file exists, and the
baked-in config wins over anything a bridge hands over. It is now an
alternative, not a prerequisite. Note that if the baked-in project and the
bridge's differ, the phone registers against one project while the bridge sends
from the other, and push fails silently.

## Out of scope (for now)

File-diff viewing, a merged sessions list across hosts, biometric lock, and
tablet-specific layouts. The bridge performs read methods, terminal
input/replay, feed replies, workspace rename, and workspace/pane create,
select and close (each confirmed on the phone where destructive) — it never
restores workspaces/terminals.
