# Constitution

The rules every change in this repo follows. Product decisions come from
[cast/AGENT_PROTOCOL.md](cast/AGENT_PROTOCOL.md#decisions) (`D1`–`D14`); this
page repeats the ones that constrain all work and adds the engineering and
working rules. If a change needs to break a rule, change this page first, in
its own commit, with the reason.

## Product

1. **Small groups, peer to peer.** The target is H.264 1080p60 game streaming
   in rooms of at most 10 people (D11). Media flows between the participants'
   PCs; the signaling server never carries audio or video, except through its
   optional TURN server for members who hide their IP (D12).
2. **The mic belongs to the user** (D2). Nothing in the core mutes, unmutes or
   changes the mic mode on its own: not on a hang, a failure, a reconnect or a
   join. Push-to-talk on first install; the app remembers the user's choice.
3. **Voice never depends on streams** (D9). Starting, stopping, watching or
   switching streams, and supernode failover, never interrupt voice or restart
   the sidecar.
4. **Every stream has its own tree** (D5). Something happening to one stream
   never changes another. Streamers are the last choice as supernode (D7).
5. **Relaying for others is the member's choice** (D13), and when upload falls
   short, extra watchers wait rather than everyone stuttering (D14). Nobody,
   the streamer least of all, is made to send more than they can carry.
6. **One session per process, controlled over stdio** (D1, D10).
7. **Out of scope until decided otherwise:** chat (D3), watching while streaming
   (D8), a media server.

## Engineering

1. **Measure, don't claim.** Latency, failover time, echo reduction and CPU
   figures come from a test or a recorded measurement. Untested paths (real
   microphones and speakers, real NAT, Windows at runtime) are labelled as
   untested.
2. **Failover stays sub-second.** A hung supernode is replaced in about
   570 ms and a leaving one in about 20 ms (measured). A change that slows
   this down is a regression; the integration tests guard it.
3. **Encode once, forward without decoding.** The streamer packetizes each
   frame once for every link; supernodes forward RTP as received. No
   transcoding in the forwarding path.
4. **Go stays cgo-free** so `cast` cross-compiles for every target. Native
   audio work (devices, echo cancellation, Opus) lives in the Rust `voice`
   sidecar.
5. **The sidecar contract is small and stable:** `READY` on stdout, one-line
   commands on stdin, exit on stdin EOF. State changes are commands, never a
   restart.
6. **Diagnostics are opt-in** (`-stats`). No periodic logging unless asked for.
7. **The protocol spec leads.** A change to the agent protocol or to a
   decision goes into `cast/AGENT_PROTOCOL.md` (decisions, change list) before
   or with the code.
8. **Green before commit:** `go vet ./... && go test ./...` in `cast/`;
   `cargo test` and `cargo clippy -- -D warnings` in `voice/`.

## Working

1. **Commits by category**, one concern each, with a scope:
   `feat(peer): …`, `fix(voice): …`, `docs: …`, `refactor: …`, `test: …`.
2. **Features on a branch** (`feat/<name>`), merged into `main` through a
   reviewed pull request.
3. **No "Generated with Claude Code" footer** in pull requests, review
   comments or other GitHub text.
4. **Toolchain:** Go 1.24; Rust via rustup (`. "$HOME/.cargo/env"`, the shell
   profile is left untouched); libopus from Homebrew on macOS, found through
   `pkg-config`.
