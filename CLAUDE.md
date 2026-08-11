# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Tunnels internet traffic through video-calling platforms (VK Call, Yandex Telemost, WB Stream, DION) so it looks like a normal video call to DPI. The platform SFU is on the government whitelist; the tunnel rides on either a WebRTC DataChannel (**DC mode**) or a published VP8 video track (**Video mode**).

Two ends, always paired:
- **Creator** — runs on free internet, dials out to the real destination
- **Joiner** — runs on the censored side, exposes a local SOCKS5 proxy (plus TUN/VpnService for whole-device capture)

Two implementation paths: **headless** (pure Go + Pion, talks to the SFU directly — this is where all new work goes) and a **legacy browser path** (Electron creator + Android `WebView` joiner with JS hooks in `hooks/`, VK DC only, being phased out). ChaCha20 obfuscation, VP8 pacing, KCP reliability, and the LiveKit/WB Stream/DION backends are headless-only.

## Build & test

Everything is a shell script at the repo root. No Makefile, no task runner.

```sh
./build-headless.sh     # 8 Go binaries (4 creators, 3 joiners, vk-bot) for the current platform — fastest inner loop
./build-go.sh           # gomobile .aar + librelay.so for Android + desktop relay + creators (needs NDK, macOS paths hardcoded)
./build-android.sh      # release APK -> prebuilts/whitelist-bypass.apk
./build-cli.sh          # cross-compiled per-arch zips of all headless binaries
./build-creator.sh      # Electron creator app, all platforms
./build-joiner-app.sh   # Electron desktop joiner app
./build-ios.sh          # Mobile.xcframework + unsigned IPA (macOS only)
./copy-hooks.sh         # sync hooks/*.js into android-app assets (legacy path)
./make-release.sh       # everything, in order
docker compose -f docker-build/docker-compose.yml up   # all components except macOS creator
```

For a plain Go compile check without the build scripts, `go -C relay build ./...` and `go -C headless/<x> build .`.

There are **no Go unit tests** in this repo. Verification is end-to-end shell smoke tests in `headless/tests/`, and each one needs real platform cookies and live network:

```sh
./build-headless.sh                                        # tests require the binaries to exist
./headless/tests/test-wbstream-joiner.sh [video|dc]        # WBSTREAM_COOKIES or ./cookies-wbstream.json
./headless/tests/test-telemost-joiner.sh
./headless/tests/test-*-join-existing.sh                   # attach to an existing call instead of creating one
UPSTREAM_SOCKS=host:port ./headless/tests/test-upstream-socks.sh <cookies-yandex.json> <tm-link>
```

Each test spawns a creator+joiner pair, waits for `join_link:` then `TUNNEL CONNECTED` in the logs, curls `api.ipify.org` through the joiner's SOCKS5, and reports throughput. When you can't run these (no cookies, no network), say so rather than claiming verification.

`cookies*.json` and `prebuilts/` are gitignored. Cookies come from the desktop creator app's `Export Cookies` button, as `[{"name":..,"value":..},...]`; DION also accepts `{"email":..,"password":..,"cookies":[]}`.

## Module layout

Go is a **multi-module** workspace, not a single module. `relay/` is the shared library (`module whitelist-bypass/relay`); every binary under `headless/*` and `joiner-desktop-app/desktop-joiner` is its own module with `replace whitelist-bypass/relay => ../../relay`. Consequences:

- A change in `relay/` is picked up by all binaries with no version bump, but each module has its **own** `go.sum` and its own pinned indirect deps — these have already drifted (e.g. `pion/webrtc` v4.2.9 in relay vs v4.2.11 in `headless/vk`). Adding a dependency to `relay/` means running `go mod tidy` in every consumer module.
- `go build ./...` from the repo root does nothing useful. Always `go -C <module-dir>`.

## Architecture

The layering is what matters here; it's spread across several packages and hard to see from any single file.

**`relay/tunnel/` — transport-agnostic core.** Everything above this layer is identical for DC and Video mode. `protocol.go` defines the framing: 4-byte length, 4-byte conn ID, 1-byte msg type (`MsgConnect`/`MsgData`/`MsgUDP`/`MsgConfig`/...), conn ID `0` reserved for control. `relay_bridge.go` is the multiplexer and the only place that knows about SOCKS5 and real sockets — one `RelayBridge` per end, `mode` string picks joiner vs creator semantics (joiner accepts SOCKS and sends `MsgConnect`; creator dials out, optionally via `--upstream-socks`). Anything satisfying `tunnel.DataTunnel` (`SendData`/`SetOnData`/`SetOnClose`/`Reconfigure`) can carry it: `DCTunnel` (SCTP, with chunking), `VP8DataTunnel` (paced frames on a video track), `MultiTrackTunnel` (fans over cam + screenshare tracks; track 0 is the cam and is the only one whose close cascades), `MultiTrackKCPTunnel` (KCP reliability over the lossy VP8 path).

**`obfuscator.go`** wraps every payload in ChaCha20-Poly1305 disguised as a VP8 keyframe/interframe header, with an epoch field for peer-restart detection. The key is derived from the join link — `tunnel.DeriveSecretFromJoinLink(<link or room id>)` — so both ends must feed it the **same** string. If you touch what gets passed in (VK join link, Telemost `ConferenceURI`, WB room UUID, DION slug), change creator, joiner, mobile bindings, and desktop joiner together or the tunnel silently fails to decode.

**`relay/pion/headless-joiner-common/` — the joiner brains, shared by all frontends.** Each platform has a `*_joiner.go` exposing `RunWithParams(jsonParams string)`. Platform-specific I/O is injected through the small interfaces in `deps.go` (`ResolveFunc`, `StatusEmitter`, `PeerConnectionConfigurer`, `CacheStore`), which is why the same joiner logic serves the CLI, Android, and iOS. Three frontends wrap it: `relay/pion/android/` (used by the `librelay.so` subprocess), `relay/pion/ios/` (gomobile xcframework), and the `headless/*-joiner` CLI mains.

**Platform API clients** live in `relay/vk`-ish siblings: `relay/telemost/api.go`, `relay/wbstream/` (+ `relay/livekit/` — WB Stream and DION are LiveKit-backed, so the LiveKit signaling client is separate from the session logic in `wbstream/session.go`), `relay/dion/`. `relay/wtsignal/` is WebTransport signaling.

**Creator side** is `headless/<platform>/main.go` — authenticate with cookies, create or join a call, build the Pion PeerConnection, hand the tunnel to a `RelayBridge` in creator mode. `headless/vk-bot/` is a VK Long Poll bot that spawns those binaries as child processes and replies with the join link; `headless/docker/` packages it (and is what `.github/workflows/docker-bot.yml` pushes to ghcr).

**Clients.** `android-app/` (Kotlin, `VpnService` + tun2socks) runs the Go relay two ways: `HeadlessRelayController` / `RelayController` spawn `librelay.so` as a **subprocess** (the `.so` is really a Go executable in `jniLibs/`) and talk to it over stdin/stdout, while the gomobile `.aar` is used for in-process bindings. `ios-proxy-app/` (Swift) is SOCKS5-only, no system VPN. `joiner-desktop-app/` is Electron over `desktop-joiner` + `relay/desktoptun/` (wintun / `/dev/net/tun` / utun; since desktop OSes have no per-process VPN exclusion, it installs `/32` bypass routes so signaling and SFU media don't loop through the tunnel).

## Cross-cutting conventions

**Process IPC is line-based text on stdout.** `common/status.go` prints `STATUS:<STATE>` and `STATUS:ERROR:<msg>`; the joiner mains print `join_link: <link>` and `TUNNEL CONNECTED`. Android/Electron parse these, and so do the smoke tests. Changing a status string or those log lines breaks all three — grep before renaming. Join params flow the other direction as one JSON line on stdin.

**`relay --mode <x>`** is the dispatch point for the multi-purpose relay binary: `dc-joiner`, `dc-creator`, `vk-video-joiner`, `vk-headless-joiner`, `telemost-headless-joiner`, `wbstream-headless-joiner`, `dion-headless-joiner`, and the `*-video-creator` variants (see the switch in `relay/main.go`).

**Resource modes** (`--resources moderate|default|unlimited|custom`) gate `--read-buf`, `--max-dc-buf`, and `--mem-limit`; those flags are ignored unless mode is `custom`, and `custom` falls back to `unlimited` values for anything unset. Uniform across creators.

**VP8 config handshake.** The joiner sends `MsgConfig` (fps, batch, trackCount) and resends every 3s until it sees `MsgConfigAck` (`config_ack.go`). Both sides must agree before the tunnel is usable, so any change to `EncodeVP8Config`/`DecodeVP8Config` is a wire-format break across every binary.

**The SFU polices the published video track, and Video mode has to keep it convinced.** Two independent obligations, both in `relay/tunnel`:
- *Keyframes.* The first payload byte is what the SFU reads as VP8's P bit, so `vp8Keepalive` (`0x30`) looks like a keyframe and `vp8Interframe` (`0xb1`) like an interframe. `EncodeData` always emits the interframe shape, so a busy tunnel publishes no keyframes at all unless something else does — `vp8tunnel.go` fires one on a 0.9–1.6s timer regardless of queue state. Starve that and Telemost flips `limitationReason` to `UNSPECIFIED` and unbinds the slot ~50s in, which reads as a one-directional tunnel death, not a media failure.
- *RTCP.* Pion buffers inbound RTCP per `RTPSender` and drops it when nobody reads, so PLI/FIR keyframe requests are invisible unless the sender is drained. `rtcp_keyframe.go` does the drain; senders are registered against their track (`RegisterRTCPSource`) because the sender exists at track creation but the tunnel that owns pacing is built later, on PeerConnection connected. A new publish path needs that one registration line or it silently loses PLI.

Keepalive padding is only legal on the keyframe shape: `Decode` treats a frame as keepalive by first byte or exact header length, so a *padded interframe* falls through to AEAD, fails on random bytes, and is dropped before epoch tracking.

**Log masking.** `common/mask.go` scrubs IPs and addresses from errors when `MaskingEnabled`. Prefer `common.MaskError(err)` over `err.Error()` in anything user-visible.

**Version is duplicated in four places** and must be bumped together: `relay/common/version.go`, `creator-app/package.json`, `joiner-desktop-app/package.json`, and `versionMajor`/`Minor`/`Patch` in `android-app/app/build.gradle.kts`.

## Notes

- Prefer Go changes in `relay/` or `headless/` over touching `hooks/*.js` — the JS path is legacy. Don't add features there.
- When editing tunnel framing, obfuscation, or the config handshake, sweep all four platforms plus the mobile bindings and the desktop joiner. There is no shared version negotiation to catch a mismatch; it just fails to connect.
- The README's build table lists `./build-app.sh`; the actual script is `./build-android.sh`.
- Requires Go 1.26+, gomobile/gobind, Android SDK + NDK 29, Java 11+, Node 18+. `build-go.sh` hardcodes macOS SDK/NDK paths (`$HOME/Library/Android/sdk`, NDK `29.0.14206865`) — on Linux/Windows use the Docker build or override the env vars.
- Committed `.exe`/`.so` build artifacts exist in the tree (`relay/relay-bundle.exe`, `headless/*-bundle.exe`, `android-app/.../librelay.so`). `build-go.sh` rewrites the `.so` files, so they show up as modified in `git status` after any build — don't commit those unless the change is intentional.
- Before committing iOS work, run `ios-proxy-app/strip-signing.sh` to drop your Apple team ID from the project file.
- `docs/SETUP.md` is the operator-facing guide and is written in Russian; keep it that way when editing.
