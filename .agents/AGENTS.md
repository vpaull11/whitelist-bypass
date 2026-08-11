# Project Guide & Context: Whitelist Bypass

This project (`whitelist-bypass`) implements internet traffic tunneling through video calling platforms (VK Call, Yandex Telemost, WB Stream, DION) to bypass government whitelist censorship.

## Key Architectural & Implementation Principles

1. **Dual Architecture (Headless vs. Legacy Browser):**
   - **Headless (Recommended):** Pure Go using Pion WebRTC directly communicating with SFU endpoints. Supports ChaCha20 obfuscation, VP8 pacing, LiveKit / WB Stream / DION backends.
   - **Legacy Browser Path:** Electron Creator app + WebView Android client using JavaScript hooks (`hooks/`). Phasing out; primary focus is Headless Go / Mobile wrappers.

2. **Core Components & Layout:**
   - `relay/`: Core Go library shared by all targets. Contains SOCKS5 server, tun2socks integration, DC/VP8 tunneling protocols, ChaCha20 obfuscation, stream multiplexer, and mobile wrapper bindings (gomobile/iOS/Android).
   - `headless/`: Go binaries for platform creators and joiners:
     - `headless/vk/`, `headless/telemost/`, `headless/wbstream/`, `headless/dion/`
     - `headless/vk-bot/` (VK Long Poll bot for creator spawning)
     - `headless/tests/` (E2E tests)
   - `android-app/`: Kotlin/Java Android app wrapping `VpnService` + `tun2socks` + Pion relay bindings (`gomobile`).
   - `ios-proxy-app/`: Swift iOS app providing local SOCKS5 proxy using `gomobile` xcframework.
   - `creator-app/`: Electron desktop application serving as a GUI for controlling creator instances.
   - `hooks/`: Legacy JS hook scripts injected into platform web applications.

3. **Build & Tooling Setup:**
   - Build scripts at root: `build-go.sh`, `build-android.sh`, `build-ios.sh`, `build-cli.sh`, `build-creator.sh`, `make-release.sh`.
   - Toolchains required: Go 1.26+, gomobile/gobind, Android NDK 29 / SDK, Java 11+, Node.js 18+.

4. **Development Guidelines:**
   - Always prioritize Go implementation changes in `relay/` or `headless/` over legacy JS hooks unless specified.
   - When modifying tunnel framing or encryption in `relay/`, ensure compatibility across all platform-specific headless implementation modules.
   - Keep platform authorization/cookies handling clean and safe when modifying `cookies-*.json` parser mechanisms.
