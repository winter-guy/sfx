# gamecast

**Low-latency game streaming to a small group, with no media server.**

gamecast streams a game from one PC to a small group of friends (about 10 people)
as H.264 1080p60 with Opus game audio over WebRTC, with voice chat between
everyone in the room. A streamer's home connection usually can't send 9 copies of a 1080p60
stream, so gamecast elects a **supernode**: one viewer on a fast connection
receives the stream once and forwards it to everyone else. Any client can be
elected, because every client runs the same forwarding code. A **warm backup** takes over in
about half a second if the supernode freezes or disappears.

The only server is a lightweight **signaling server** that coordinates peers.
It never touches video.

> **Status: working prototype.** The media core (election, forwarding, failover)
> game audio and voice chat are implemented and covered by automated tests.
> Video capture and playback currently rely on `ffmpeg`/`ffplay`, voice has only
> been tested with synthetic audio (not real microphones and speakers), and
> nothing has been tested between machines on different networks. See [Project status](#project-status).

---

## Contents

- [Highlights](#highlights)
- [Project status](#project-status)
- [How it works](#how-it-works)
  - [Components](#components)
  - [Roles](#roles)
  - [The media path](#the-media-path)
  - [Links: live and standby](#links-live-and-standby)
  - [Supernode election](#supernode-election)
  - [Failure detection and failover](#failure-detection-and-failover)
  - [Keyframes](#keyframes)
  - [Voice chat](#voice-chat)
- [Requirements](#requirements)
- [Getting started](#getting-started)
- [Command reference](#command-reference)
- [Encoder settings](#encoder-settings)
- [Networking and deployment](#networking-and-deployment)
- [Signaling protocol](#signaling-protocol)
- [Tuning constants](#tuning-constants)
- [Logs and diagnostics](#logs-and-diagnostics)
- [Testing](#testing)
- [Project layout](#project-layout)
- [Design decisions](#design-decisions)
- [Limitations](#limitations)
- [Security](#security)
- [Roadmap](#roadmap)
- [Troubleshooting](#troubleshooting)
- [FAQ](#faq)
- [Contributing](#contributing)
- [License](#license)

---

## Highlights

- **No media server.** A viewer's PC forwards the stream; the signaling server
  only exchanges small JSON messages.
- **Encode once.** The streamer encodes each frame once and sends the same
  packets to every peer it serves.
- **Forward without re-encoding.** The supernode passes RTP packets through
  untouched: no decode, no quality loss, little CPU.
- **Game audio.** An Opus track travels the same links as the video, is
  forwarded the same way and switches with it on failover.
- **Voice chat with echo cancellation.** A peer-to-peer voice mesh, separate
  from the video tree, so supernode failovers never cut voice. The audio work
  (mic, echo cancellation, noise suppression, gain control, Opus, mixing) runs
  in a small Rust sidecar built on [sonora](https://crates.io/crates/sonora),
  a pure-Rust port of WebRTC's audio processing.
- **Automatic supernode election** based on upload bandwidth, wired vs Wi-Fi
  and opt-in, with a degraded mode when nobody qualifies.
- **Sub-second failover.** A warm standby backup is pre-connected; switching is a
  track swap with no new handshakes.
  - Supernode leaves or crashes: picture back in **~20 ms**
  - Supernode freezes or loses its network: picture back in **~570 ms**
- **No false alarms when the streamer pauses.** If the game or encoder stalls,
  the server knows the problem is upstream and doesn't switch hosts.
- **Small footprint.** The supernode uses about 16% of one CPU core and 50 MB of
  memory when forwarding 8.7 Mbps to 7 viewers.
- **Pure Go, no cgo.** It builds for macOS, Windows and Linux.

## Project status

| Area | State | How it was verified |
|---|---|---|
| Supernode election (host + backup) | ✅ Done | Unit tests |
| Signaling server, rooms, SDP relay | ✅ Done | Integration tests, real binaries on one machine |
| RTP forwarding through the supernode | ✅ Done | Integration tests (synthetic H.264), real binaries |
| Warm-standby failover | ✅ Done | Tests for hang, clean leave, and a streamer pause that must not trigger failover |
| Keyframe requests (PLI) end to end | ✅ Done | Integration tests |
| Game audio (Opus), incl. through failover | ✅ Done | Integration tests; real binaries with synthetic Opus RTP over UDP. Real ffmpeg audio and actual listening are untested |
| Resource diagnostics (`-stats`) | ✅ Done | Unit test, real binaries |
| Real video (ffmpeg capture → ffplay) | ⚠️ Untested | ffmpeg was not available on the development machine; tests use synthetic H.264 |
| Peers on different networks (NAT, TURN) | ⚠️ Untested | Only tested over loopback |
| Windows | ⚠️ Compiles only | Passes `go vet` for Windows, never run there |
| Voice chat (Rust sidecar, sonora echo cancellation) | ✅ Done | Rust unit tests (76 dB echo reduction in simulation); Go end-to-end tests with the real sidecar (64 dB echo reduction, mute, game audio); CLI smoke test. **Real microphones and speakers untested** |
| Native capture/encoding, in-app player, UI | ❌ Not started | See [Roadmap](#roadmap) |

---

## How it works

### Components

```
                    ┌──────────────────────────────┐
                    │       signaling server       │   rooms, election,
                    │  (gamecast signal, no media) │   health monitor, SDP relay
                    └──────────────┬───────────────┘
                                   │ WebSocket (JSON)
        ┌──────────────────────────┼──────────────────────────────┐
        │                          │                              │
┌───────┴───────┐  1 stream  ┌─────┴──────┐   N-1 copies   ┌──────┴──────┐
│   Streamer    │ ─────────▶ │ Supernode  │ ─────────────▶ │   Viewers   │
│ capture+encode│   (RTP)    │ (a viewer) │     (RTP)      │ decode+play │
└───────┬───────┘            └────────────┘                └──────▲──────┘
        │                                                         │
        │ standby (connected, idle) ┌────────────┐  standby       │
        └─────────────────────────▶ │   Backup   │ ───────────────┘
                                    │ (a viewer) │
                                    └────────────┘
```

| Component | Command | What it does |
|---|---|---|
| Signaling server | `gamecast signal` | Tracks rooms and peers, elects the supernode and backup, watches the supernode's health, relays SDP offers/answers, serves an upload speed test. Never handles media. |
| Streamer | `gamecast stream` | Reads encoded H.264 frames (currently from an ffmpeg pipe) and, optionally, Opus game audio (RTP on a local UDP port). Sends both on its live links. |
| Viewer | `gamecast watch` | Receives the stream and writes video (H.264) and audio (Ogg/Opus) out for playback (currently ffplay). It may be elected supernode or backup. |

### Roles

The server assigns roles each time the room changes. Roles are announced in a
**topology** message that every peer receives.

| Role | Who | Responsibility |
|---|---|---|
| **Streamer** | The one `gamecast stream` in the room | Source of the stream. Sends to the host (and holds a standby link to the backup). |
| **Host** (supernode) | The streamer itself, or a viewer that opted in with `-host` | Receives from the streamer and forwards to every other viewer. |
| **Backup** | Another opted-in viewer, or the streamer | Pre-connected warm standby. Takes over instantly if the host fails. |
| **Viewer** | Everyone else | Plays the stream from the host. |

The streamer can be its own host. This happens when it has enough upload to serve
everyone, or when there's only one viewer. In that case there is no backup: if
the streamer disappears, there is no stream to save.

### The media path

1. **Encode once.** The streamer writes each encoded frame to a single
   `TrackLocalStaticSample`. Pion packetizes it into RTP once, and the same
   packets go to every live link. The cost of encoding doesn't grow with the
   number of viewers.
2. **Forward untouched.** The supernode reads RTP packets from the streamer and
   writes them to a `TrackLocalStaticRTP` bound to every viewer link. It never
   decodes or re-encodes. The only change is removing RTP header extensions,
   whose IDs were negotiated with the upstream peer and mean nothing downstream.
3. **Encrypted per hop.** Every link is a normal WebRTC PeerConnection (DTLS-SRTP),
   so packets are decrypted and re-encrypted per viewer on the supernode. This
   per-viewer encryption is most of the supernode's CPU cost.
4. **Retransmissions are local.** NACK handling runs on each link, so a lost
   packet on a viewer's connection is re-sent by the supernode, not by the streamer.

5. **Game audio rides along.** Every link carries a second track: Opus,
   48 kHz stereo. The streamer forwards Opus RTP packets from its encoder as-is
   (one packet per 10–20 ms). The supernode forwards them like video. Live/standby
   swaps and failover switch audio and video together. Each viewer writes one
   continuous Ogg/Opus stream; positions in it come from the Opus packets
   themselves, so a supernode switch only leaves a gap equal to the failover
   time.

All peers negotiate exactly one video codec (H.264, packetization mode 1,
payload type 102) and one audio codec (Opus, payload type 111). Every link
negotiates audio even when the streamer has no audio input. Both ends are always gamecast, so the SDP profile string is pinned. The
bitstream itself can be any profile the encoder produces, because it is
forwarded as-is.

### Links: live and standby

Each peer keeps a set of persistent **links**, each a one-way PeerConnection. The
side that has (or will have) the stream always sends the offer. When the
topology changes, each peer works out which links it needs, opens missing ones,
closes stale ones and switches which ones carry media.

| Link | State | Carries |
|---|---|---|
| streamer → host | live | The stream |
| streamer → backup | standby | An idle track that is never written. Swapped for the real track on failover. |
| host → every viewer | live | Whatever the host receives from the streamer |
| backup → every viewer | connected, silent | Nothing until the streamer starts feeding the backup |

If the backup is the streamer itself (no viewer can take over), the streamer
holds standby links to every viewer and can fall back to sending to everyone
directly.

Standby links cost almost nothing: ICE keepalives and RTCP only.

### Supernode election

A candidate qualifies as host if:

```
upload ≥ 1.3 × bitrate × copies it must send
```

- `1.3` is headroom for packet overhead, retransmissions and action-scene peaks.
- `bitrate` is the video bitrate plus the audio bitrate when the streamer has
  audio (8000 + 128 kbps by default).
- The streamer as host sends **N** copies (one per viewer).
- A viewer as host sends **N − 1** copies, because it's also a viewer.

**Election order** ([election.go](internal/signaling/election.go)):

0. **One viewer:** the streamer sends directly. It uploads one copy either way, so a relay would only add a hop.
1. **Keep the current host** while it still qualifies, to avoid needless switches.
2. **The streamer**, if it can serve everyone itself (one hop less latency).
3. **The best opted-in viewer** that qualifies: wired first, then most upload, then whoever joined earliest.
4. **Nobody qualifies:** degraded mode. Pick whoever has the most spare upload and flag the topology `degraded`.

The **backup** is elected with the same rules, excluding the host. It's sized as
if the failed host stayed in the room, so it still qualifies after a failover.
If no viewer can be backup, the streamer is the backup. A host that was declared
failed can't be elected again for 30 seconds.

**Required upload at 8 Mbps (H.264 1080p60):**

| Viewers | Viewer as host | Streamer as host |
|---|---|---|
| 2 | 10.4 Mbps | 20.8 Mbps |
| 3 | 20.8 Mbps | 31.2 Mbps |
| 4 | 31.2 Mbps | 41.6 Mbps |
| 5 | 41.6 Mbps | 52.0 Mbps |
| 6 | 52.0 Mbps | 62.4 Mbps |
| 7 | 62.4 Mbps | 72.8 Mbps |
| 8 | 72.8 Mbps | 83.2 Mbps |
| 9 | 83.2 Mbps | 93.6 Mbps |

### Failure detection and failover

Every client sends a **status heartbeat** over the WebSocket every 250 ms. It
says which peer the client plays from and whether packets are arriving:

| State | Meaning |
|---|---|
| `flowing` | Packets are arriving |
| `waiting` | The link just became active; no packet yet |
| `stalled` | No packet for 400 ms, or an active link delivered nothing for 5 s |
| `none` | Not receiving (the streamer, or no host) |

The server checks the host every 100 ms and declares it **failed** when:

| Condition | Typical cause |
|---|---|
| Its WebSocket closes | Process exited or crashed (the OS resets the connection) |
| Most of its viewers are `stalled` **and** no heartbeat for 500 ms | Frozen process, network loss, power cut |
| No heartbeat for 1.5 s | Same, when no viewer is reporting |
| Host reports `flowing`, but most viewers have been `stalled` for 1.5 s | Host's upload saturated or path broken |

If the **host itself** reports that it isn't receiving, the streamer is the
problem (game paused, encoder stuck). Switching hosts wouldn't help, so the
server does nothing.

**What happens on failover:**

```
t=0      supernode freezes (sockets stay open, nothing is sent)
~400ms   viewers' video stalls; their next heartbeat says "stalled"
~520ms   server: most viewers stalled + no heartbeat for >500ms → host failed
         server: backup promoted, new backup elected, topology broadcast
~520ms   streamer swaps the real track onto the backup link, forces a keyframe
~570ms   backup forwards; viewers switch playback to the backup link → picture
```

**Measured:**

| Event | Picture back on viewers | Where |
|---|---|---|
| Supernode leaves or crashes | ~20 ms | Integration test (`-race`) |
| Supernode freezes (no TCP reset) | ~570 ms | Integration test (`-race`) |
| Supernode frozen with `SIGSTOP` | Declared failed after 517 ms, viewers switched in the same millisecond | Real binaries on one machine |
| Streamer paused for 3 s | No failover (correct) | Integration test |

Over the internet, add one signaling round trip (about 20–100 ms). With the piped
ffmpeg encoder, viewers also wait for the next scheduled keyframe. See
[Keyframes](#keyframes).

### Keyframes

A viewer can only start decoding H.264 at a keyframe (IDR). Keyframe requests
travel as RTCP PLI messages:

```
viewer ──PLI──▶ supernode ──PLI──▶ streamer ──▶ encoder: "next frame is a keyframe"
```

- Requests are throttled to **one per 500 ms** per hop, because every keyframe is
  large and briefly raises the bitrate for everyone. A request that arrives inside
  the window is delayed to the end of the window, not dropped.
- A backup that isn't receiving never forwards keyframe requests, so standby
  links can't cause keyframe spam.
- When the host changes, the streamer **forces a keyframe immediately**,
  without waiting for viewers to ask.
- **Limitation:** an encoder behind a pipe (ffmpeg today) can't receive these
  requests. Viewers get a picture at the encoder's next scheduled keyframe, so
  keep the keyframe interval short (`-g 30`, 0.5 s). The planned native encoder
  will honor requests immediately.

### Voice chat

Voice is split between two processes. **gamecast (Go)** carries the packets;
the **gamecast-voice sidecar (Rust)** does all the audio work:

```
            gamecast (Go)                          gamecast-voice (Rust sidecar)
  ┌───────────────────────────────┐   UDP   ┌──────────────────────────────────────┐
  │ voice mesh: 1 PeerConnection  │ ◀────── │ mic ─▶ sonora: echo cancel, noise   │
  │ per pair of voice peers,      │  Opus   │        suppression, AGC ─▶ Opus 32k │
  │ Opus RTP both ways            │   RTP   │                                      │
  │                               │ ──────▶ │ voices ─▶ jitter buffer (per SSRC) ─┐│
  │ game audio (from video tree)  │ ──────▶ │ game   ─▶ jitter buffer ────────────┤│
  └───────────────────────────────┘         │            mix ─▶ speakers  ◀───────┘│
                                            │              └──▶ echo reference      │
                                            └──────────────────────────────────────┘
```

- **Mesh, not the supernode tree.** Every voice peer has one sendrecv audio
  PeerConnection to every other voice peer (the smaller peer ID offers). Voice
  is about 32 kbps per stream, so even 10 people need only ~0.3 Mbps up each.
  Because it doesn't depend on the supernode, a video failover never interrupts
  voice.
- **Encode once.** Each peer's mic is encoded once by the sidecar and the same
  packets go out on every voice link.
- **Echo cancellation sees everything you hear.** The sidecar plays voices *and*
  game audio, so both are in the echo canceller's reference signal and your mic
  doesn't send them back. On the **streamer**, the game is already playing
  through the OS, so the sidecar gets game audio as *reference only*
  (`--game reference`), not to play.
- **Jitter buffer** per speaker: reorders packets, conceals short gaps with
  Opus packet loss concealment, skips long gaps, and caps buffering at 200 ms.
- **Devices:** the default microphone and speakers via cpal (CoreAudio, WASAPI,
  ALSA), resampled to 48 kHz as needed.
- **Process protocol:** the sidecar prints one handshake line on stdout
  (`READY voice=127.0.0.1:P game=127.0.0.1:Q`), takes `mute`, `unmute` and `quit`
  on stdin, quits when stdin closes (so it never outlives gamecast), and logs to
  stderr, which gamecast forwards to its own log.
- **Mute** from Go: `Client.SetVoiceMuted(bool)`; from the CLI: `-voice-muted`
  at startup.

---

## Requirements

### To build

- Go **1.24** or newer. No cgo, no C libraries.
- **For voice chat only:** Rust (via [rustup](https://rustup.rs)) and libopus
  (`brew install opus`, `apt install libopus-dev`), found with pkg-config. Or
  build with `--features bundled`, which compiles libopus from source and links
  it statically (needs cmake); that's the better choice for release builds.

### To run (for now)

- **ffmpeg** on the streamer, to capture and hardware-encode the screen, and to
  encode game audio to Opus
- **ffplay** (part of ffmpeg) on viewers, to display the video and play the audio
- **For game audio, a way to capture system audio:** ffmpeg can't record what
  your PC plays out of the box.
  - **macOS:** a virtual loopback device, such as BlackHole
  - **Windows:** "Stereo Mix" (if your sound driver has it) or a virtual audio
    capture device
  - **Linux:** the PulseAudio/PipeWire monitor source

  Native capture (WASAPI loopback / ScreenCaptureKit) is on the roadmap.

### Streamer

The streamer needs a **GPU hardware H.264 encoder**. Software encoding of 1080p60
would take 4–8 CPU cores away from the game.

| Vendor | Hardware H.264 encoder |
|---|---|
| NVIDIA | GTX 10 series and newer (RTX 20+ gives noticeably better quality) |
| AMD | RX 400 and newer (RX 6000+ recommended) |
| Intel | 6th-gen Core and newer (Quick Sync), Arc GPUs |
| Apple | All Apple Silicon (VideoToolbox) |

Upload needed by the streamer:

| Setup | Upload |
|---|---|
| A viewer is the supernode | ≥ 15 Mbps (one 8 Mbps stream, with headroom) |
| Streamer serves everyone itself | See the election table above (≈ 94 Mbps for 9 viewers) |

Keep the stream at no more than about 80% of measured upload, so the streamer's
own game traffic isn't delayed.

### Supernode

| Resource | Needed for 9 viewers at 8 Mbps |
|---|---|
| Upload | ≥ 83 Mbps (fiber in practice) |
| Download | ≥ 10 Mbps |
| CPU | Any dual-core or better; forwarding used ~16% of one core in tests |
| Memory | ~50 MB |
| Network | Wired Ethernet strongly preferred; a router with SQM/QoS helps |
| Data | ~29 GB uploaded per hour (8 copies × 8 Mbps) |

Connection types, roughly:

| Connection | Typical upload | Can be supernode for 9? |
|---|---|---|
| Fiber (symmetric) | 300–1000 Mbps | ✅ Yes |
| Cable, high tier | 50–200 Mbps | ⚠️ Only plans with 100+ Mbps upload |
| Cable, standard | 10–40 Mbps | ❌ |
| 5G home, Starlink, DSL | 1–50 Mbps | ❌ |

Watch for ISP data caps: hosting 2 hours a day is roughly 1.7 TB a month.

### Viewer

Any PC that can decode H.264 1080p60 in hardware (practically every PC from the
last decade), with at least 10 Mbps download.

---

## Getting started

### Build

```sh
cd gamecast
go build -o bin/gamecast ./cmd/gamecast

# Voice chat sidecar (optional). gamecast looks for it next to its own binary.
(cd voice && cargo build --release) && cp voice/target/release/gamecast-voice bin/

go test -race ./...                 # voice tests are skipped if the sidecar isn't built
(cd voice && cargo test)
```

### Try it on one machine

This runs everything locally over loopback. `-loopback` allows 127.0.0.1
connections, and `-ice=` disables STUN, which local peers don't need.

> The ffmpeg commands below haven't been verified as part of this repo's tests.

```sh
# 1. Make a 20-second 1080p60 test clip (no B-frames, keyframe every 0.5s)
ffmpeg -f lavfi -i testsrc2=size=1920x1080:rate=60 -t 20 \
  -c:v libx264 -preset veryfast -tune zerolatency -bf 0 -g 30 -b:v 8M \
  -f h264 test.h264

# 2. Signaling server
./bin/gamecast signal -addr 127.0.0.1:8080

# 3. Streamer, replaying the clip forever, reporting weak upload so a supernode is needed
./bin/gamecast stream -server ws://127.0.0.1:8080/ws -room demo -loopback -ice= \
  -in test.h264 -loop -upload 10

# 4. A viewer that volunteers as supernode
./bin/gamecast watch -server ws://127.0.0.1:8080/ws -room demo -loopback -ice= \
  -host -upload 200 -wired -out - | ffplay -fflags nobuffer -flags low_delay -framedrop -f h264 -

# 5. A backup and a plain viewer
./bin/gamecast watch -server ws://127.0.0.1:8080/ws -room demo -loopback -ice= \
  -host -upload 100 -out "" &
./bin/gamecast watch -server ws://127.0.0.1:8080/ws -room demo -loopback -ice= \
  -out - | ffplay -fflags nobuffer -flags low_delay -framedrop -f h264 -
```

**With game audio** (a 440 Hz test tone), restart the streamer with `-audio-in`
and feed it Opus RTP:

```sh
./bin/gamecast stream -server ws://127.0.0.1:8080/ws -room demo -loopback -ice= \
  -in test.h264 -loop -upload 10 -audio-in 127.0.0.1:5004

ffmpeg -re -f lavfi -i "sine=frequency=440:sample_rate=48000" -ac 2 \
  -c:a libopus -b:a 128k -application lowdelay \
  -f rtp -sdp_file audio.sdp rtp://127.0.0.1:5004
```

Then have a viewer write audio to a named pipe that ffplay reads (macOS/Linux).
Start ffplay first; gamecast waits until the pipe has a reader:

```sh
mkfifo audio.ogg
ffplay -nodisp -fflags nobuffer audio.ogg &
./bin/gamecast watch -server ws://127.0.0.1:8080/ws -room demo -loopback -ice= \
  -out - -audio-out audio.ogg | ffplay -fflags nobuffer -flags low_delay -framedrop -f h264 -
```

On Windows, write audio to a file (`-audio-out game.ogg`), or send audio to
stdout (`-audio-out -`) and video to a file.

**Voice chat without a microphone:** two viewers talk using synthetic audio.
Bob's playback is recorded to a raw file, so no devices or permissions are needed:

```sh
./bin/gamecast watch -server ws://127.0.0.1:8080/ws -room demo -loopback -ice= \
  -out "" -voice -voice-input sine:440 -voice-output null &
./bin/gamecast watch -server ws://127.0.0.1:8080/ws -room demo -loopback -ice= \
  -out "" -voice -voice-input silence -voice-output raw:bob.raw

ffplay -f f32le -ar 48000 -ch_layout stereo bob.raw   # Bob heard a 440 Hz tone
```

With real devices, just add `-voice`. The default microphone and speakers are
used (macOS asks for microphone permission the first time).

To see failover, freeze the supernode with `kill -STOP <pid>`. The plain viewer's
picture should continue through the backup, and the server log shows
`host … declared failed`.

### A real session

1. **Run the signaling server** somewhere everyone can reach: a small VPS, or one
   of the PCs with port 8080 forwarded.

   ```sh
   gamecast signal -addr :8080
   ```

2. **Streamer:** one ffmpeg process captures the screen and system audio. It
   pipes H.264 to gamecast and sends Opus as RTP to gamecast's audio port (see
   [Encoder settings](#encoder-settings) for other GPUs and Windows):

   ```sh
   # macOS with the BlackHole loopback device
   # (list devices with: ffmpeg -f avfoundation -list_devices true -i "")
   ffmpeg -f avfoundation -framerate 60 -capture_cursor 1 -i "1:BlackHole 2ch" \
     -map 0:v -vf scale=1920:1080 -c:v h264_videotoolbox -realtime 1 \
     -b:v 8M -maxrate 12M -bf 0 -g 30 -f h264 pipe:1 \
     -map 0:a -c:a libopus -b:a 128k -application lowdelay \
     -f rtp -sdp_file audio.sdp rtp://127.0.0.1:5004 \
   | gamecast stream -server ws://SIGNAL_HOST:8080/ws -room friday -audio-in 127.0.0.1:5004
   ```

   `-sdp_file` matters: without it, ffmpeg prints the RTP session description to
   stdout, into the video pipe. To stream without audio, use `-i "1:none"`, drop
   the `-map` options and the audio output, and leave out `-audio-in`.

   Add `-voice` to join voice chat. For the streamer this also echo-cancels the
   game sound picked up by the mic.

3. **Viewers on fast, wired connections** volunteer as supernode:

   ```sh
   gamecast watch -server ws://SIGNAL_HOST:8080/ws -room friday -host -wired -out - \
   | ffplay -fflags nobuffer -flags low_delay -framedrop -f h264 -
   ```

4. **Everyone else:**

   ```sh
   gamecast watch -server ws://SIGNAL_HOST:8080/ws -room friday -out - \
   | ffplay -fflags nobuffer -flags low_delay -framedrop -f h264 -
   ```

   For game audio, either add `-voice` (the voice sidecar plays game audio
   through your speakers, with echo cancellation) or add `-audio-out audio.ogg`
   and play it as shown in the local demo.

If peers can't connect directly (strict NAT or firewalls), add a TURN server
with `-ice`. See [Networking and deployment](#networking-and-deployment).

---

## Command reference

```
gamecast signal [-addr :8080]
gamecast stream -server ws://HOST:8080/ws -room NAME [-in -|file.h264] [flags]
gamecast watch  -server ws://HOST:8080/ws -room NAME [-host] [-out -|file.h264] [flags]
```

All commands stop cleanly on Ctrl-C / SIGTERM. Logs go to stderr; with
`watch -out -`, stdout carries only video.

### `gamecast signal`

| Flag | Default | Description |
|---|---|---|
| `-addr` | `:8080` | Listen address. Serves `/ws` (signaling) and `/speedtest`. |
| `-stats` | off | Diagnostics: log this process's CPU, memory and goroutines at this interval, e.g. `5s`. |

### `gamecast stream` and `gamecast watch`

Both commands accept the same flag set. Flags marked *stream* or *watch* only
apply to that command.

| Flag | Default | Applies to | Description |
|---|---|---|---|
| `-server` | `ws://localhost:8080/ws` | both | Signaling server URL (`ws://` or `wss://`). |
| `-room` | `default` | both | Room name. One streamer per room. |
| `-name` | hostname | both | Display name shown in topology messages. |
| `-upload` | `0` | both | Upload in Mbps used for the election. `0` measures it against the signaling server for 3 s (only for the streamer and `-host` viewers). |
| `-wired` | `false` | both | This PC is on wired Ethernet. Preferred when electing a supernode. |
| `-ice` | `stun:stun.l.google.com:19302` | both | Comma-separated STUN/TURN URLs. TURN with credentials: `turn:user:pass@host:3478`. Empty (`-ice=`) disables STUN. |
| `-loopback` | `false` | both | Allow 127.0.0.1 candidates. Single-machine demos only. |
| `-stats` | off | both | Diagnostics: log this process's resource use at this interval. |
| `-in` | `-` | stream | H.264 Annex-B input: `-` for stdin (a live encoder) or a file path. |
| `-loop` | `false` | stream | Replay the `-in` file forever (for testing without an encoder). |
| `-fps` | `60` | stream | Frame rate. Used to pace files and as the nominal frame duration. |
| `-bitrate` | `8000` | stream | Encoder target in kbps. Only used to size the supernode; it doesn't change the encoder. Keep it in line with ffmpeg's `-b:v`. |
| `-audio-in` | none | stream | Receive game audio as Opus RTP on this UDP address, e.g. `127.0.0.1:5004`. Port+1 is also bound to absorb the encoder's RTCP. Empty = no audio. |
| `-audio-bitrate` | `128` | stream | Opus bitrate in kbps, added to `-bitrate` when sizing the supernode. Keep it in line with ffmpeg's `-b:a`. |
| `-host` | `false` | watch | Volunteer this PC as supernode/backup. |
| `-out` | `-` | watch | Where to write received H.264: `-` for stdout, a file path, or `""` to discard. |
| `-voice` | `false` | both | Join voice chat. Starts the gamecast-voice sidecar; on viewers it also plays game audio. |
| `-voice-bin` | auto | both | Path to `gamecast-voice`. Default: next to the gamecast binary, then `$PATH`. |
| `-voice-input` | `default` | both | Microphone: `default`, `sine:FREQ` (test tone) or `silence`. |
| `-voice-output` | `default` | both | Playback: `default` (speakers), `raw:PATH` (f32le 48 kHz stereo file) or `null`. |
| `-voice-muted` | `false` | both | Start with the microphone muted. |
| `-audio-out` | none | watch | Where to write received game audio as Ogg/Opus: a file, a named pipe, or `-` for stdout (only if `-out` isn't `-`). Empty = don't write audio. |

### `gamecast-voice` (sidecar)

Normally started by gamecast, but it can be run alone for diagnosis:

```
gamecast-voice --send HOST:PORT [options]
gamecast-voice --list-devices          # show the default devices, without opening them
```

| Flag | Default | Description |
|---|---|---|
| `--send` | required | Where to send mic audio (Opus RTP) |
| `--input` | `default` | `default`, `sine:FREQ` or `silence` |
| `--output` | `default` | `default`, `raw:PATH` or `null` |
| `--game` | `play` | `play`, `reference` (echo-cancel only; used on the streamer) or `off` |
| `--bitrate` | `32000` | Opus bitrate for the mic |
| `--muted` | off | Start muted |
| `--no-aec`, `--no-ns`, `--no-agc` | on | Disable echo cancellation, noise suppression or automatic gain control |
| `--echo-sim G:MS` | off | Test aid: leak G × the speaker output into the mic MS ms later |

Input pacing: from stdin, frames are sent as they arrive (the encoder sets the
pace). From a file, frames are paced at `-fps`.

---

## Encoder settings

These settings matter more than anything else for latency and failover speed:

| Setting | Value | Why |
|---|---|---|
| B-frames | **`-bf 0`** | B-frames reorder frames and add a frame or more of delay. |
| Keyframe interval | **`-g 30`** (0.5 s at 60 fps) | A piped encoder can't be asked for keyframes, so this interval caps how fast new viewers and failovers get a picture. |
| Bitrate | `-b:v 8M -maxrate 12M` | 8 Mbps for 1080p60 H.264; peaks capped at 1.5×. Match `gamecast stream -bitrate`. |
| Rate control | CBR or capped VBR | Predictable bandwidth for the supernode. |
| Output | `-f h264 -` | Raw Annex-B H.264 on stdout. |

**Platform examples** (ffmpeg 6+; not yet verified in this repo):

```sh
# macOS, Apple Silicon (VideoToolbox)
ffmpeg -f avfoundation -framerate 60 -capture_cursor 1 -i "1:none" \
  -vf scale=1920:1080 -c:v h264_videotoolbox -realtime 1 \
  -b:v 8M -maxrate 12M -bf 0 -g 30 -f h264 -

# Windows, NVIDIA (NVENC)
ffmpeg -f lavfi -i ddagrab=framerate=60 -c:v h264_nvenc -preset p1 -tune ull \
  -rc cbr -b:v 8M -maxrate 12M -bufsize 8M -bf 0 -g 30 -f h264 -

# Windows, AMD (AMF)
ffmpeg -f lavfi -i ddagrab=framerate=60 -c:v h264_amf -usage ultralowlatency \
  -rc cbr -b:v 8M -maxrate 12M -bf 0 -g 30 -f h264 -

# Windows, Intel (Quick Sync)
ffmpeg -f lavfi -i ddagrab=framerate=60 -c:v h264_qsv -preset veryfast \
  -b:v 8M -maxrate 12M -bf 0 -g 30 -f h264 -
```

Pipe any of these into `gamecast stream -server … -room …`.

### Game audio

| Setting | Value | Why |
|---|---|---|
| Codec | `-c:a libopus` | The only audio codec gamecast negotiates |
| Bitrate | `-b:a 128k` | Good quality for game audio; match `-audio-bitrate` |
| Mode | `-application lowdelay` | Lowest-latency Opus mode |
| Output | `-f rtp -sdp_file audio.sdp rtp://127.0.0.1:5004` | RTP to gamecast's `-audio-in`; `-sdp_file` keeps the SDP out of stdout |

Add the audio output to the same ffmpeg command as the video, with `-map` to
pick streams. Windows example with an audio capture device:

```sh
ffmpeg -f lavfi -i ddagrab=framerate=60 -f dshow -i audio="Stereo Mix (Realtek(R) Audio)" \
  -map 0:v -c:v h264_nvenc -preset p1 -tune ull -rc cbr -b:v 8M -maxrate 12M -bufsize 8M \
  -bf 0 -g 30 -f h264 pipe:1 \
  -map 1:a -c:a libopus -b:a 128k -application lowdelay \
  -f rtp -sdp_file audio.sdp rtp://127.0.0.1:5004 \
| gamecast stream -server … -room … -audio-in 127.0.0.1:5004
```

Why RTP over local UDP for audio input? The video already uses stdin, and UDP
works the same on Windows, macOS and Linux with no named pipes and no container
buffering. Each Opus packet arrives as soon as it's encoded.

Parsing a pipe adds about one frame (~16 ms) of delay, because a NAL unit's end
is only known when the next start code arrives. The native encoder integration
will hand over whole frames and remove this.

---

## Networking and deployment

### Ports

| What | Port | Protocol |
|---|---|---|
| Signaling server | `-addr` (default 8080) | TCP (HTTP/WebSocket) |
| Media between peers | Random high ports | UDP |
| STUN | 19302 (Google default) | UDP |
| TURN (optional) | Usually 3478 / 443 | UDP/TCP |

### NAT traversal

Peers connect directly using ICE. STUN lets peers behind ordinary home routers
discover their public addresses. Peers behind **symmetric NAT** or strict
firewalls (some corporate, university and mobile networks) need a **TURN** relay:

```sh
gamecast watch … -ice "stun:stun.l.google.com:19302,turn:user:pass@turn.example.com:3478"
```

TURN relays traffic through a server, which costs that server bandwidth. A
supernode behind TURN relays 8 copies of the stream through it, so pick
supernodes that connect directly.

SDP is sent **after ICE gathering completes** (no trickle ICE). This keeps the
code simple but adds gathering time (typically 50–300 ms with STUN) to every new
link. With warm standby, this is never on the failover path.

### Hosting the signaling server

- It's light: about 0.5% of one core and 13 MB of memory in tests. The cheapest
  VPS is plenty.
- **TLS:** the server speaks plain HTTP. For `wss://`, put it behind a reverse
  proxy that terminates TLS (Caddy, nginx) and forwards WebSocket upgrades.
  Clients accept `wss://` URLs, and the speed test then uses `https://`. (Not yet
  tested end to end.)
- It holds no persistent state. Restarting it drops all rooms; clients must reconnect.

---

## Signaling protocol

JSON messages over one WebSocket per client (`/ws`). Defined in
[proto.go](internal/proto/proto.go).

| Type | Direction | Purpose |
|---|---|---|
| `join` | client → server | First message: room, role, name, upload, wired, `canHost`, bitrate |
| `welcome` | server → client | Assigned peer ID |
| `topology` | server → all in room | Streamer, host, backup, viewers, names, degraded flag, reason |
| `signal` | client → server → client | SDP offer/answer, tagged with a link ID |
| `status` | client → server, every 250 ms | Heartbeat: upstream peer and `flowing`/`waiting`/`stalled`/`none` |
| `error` | server → client | Fatal error (for example, a second streamer joining the same room) |

**Examples:**

```json
{"type":"join","join":{"room":"friday","role":"viewer","name":"ana","uploadMbps":450,"wired":true,"canHost":true}}

{"type":"topology","epoch":3,"topology":{
  "version":7,"epoch":3,"streamer":"p1","host":"p2","backup":"p4",
  "viewers":["p2","p3","p4","p5"],"names":{"p1":"sam","p2":"ana"},
  "bitrateKbps":8000,"degraded":false,"reason":"supernode p2 (450/31 Mbps)"}}

{"type":"signal","to":"p3","link":"p2>p3#4","signal":{"kind":"offer","sdp":"v=0…"}}

{"type":"status","status":{"upstream":"p2","state":"flowing"}}
```

**Ordering rules:**

- `topology.version` increases with every update; clients ignore older versions.
- `topology.epoch` increases when the host or streamer changes. It's informational.
- Link IDs (`<from>><to>#<n>`) are chosen by the sender. Answers echo them so
  stale answers are ignored.
- An offer can arrive before the topology that allows it. Clients keep the
  latest offer per sender and accept it once the topology catches up.

**HTTP:** `POST /speedtest` reads the request body and returns
`{"mbps":…, "bytes":…}`, the rate at which the server received it.

---

## Tuning constants

| Constant | Value | File | Effect |
|---|---|---|---|
| `HeadroomFactor` | 1.3 | [election.go](internal/signaling/election.go) | Upload margin required per copy |
| `DefaultBitrateKbps` | 8000 | election.go | Bitrate assumed when the streamer doesn't say |
| `statusInterval` | 250 ms | [client.go](internal/peer/client.go) | Heartbeat interval |
| `stallAfter` | 400 ms | client.go | No packets for this long = `stalled` |
| `neverArrivedAfter` | 5 s | client.go | Active link that never delivered = `stalled` |
| `keyframeMinInterval` | 500 ms | client.go | Keyframe request throttle per hop |
| `reofferDelay` | 1 s | client.go | Wait before rebuilding a failed link |
| `monitorInterval` | 100 ms | [server.go](internal/signaling/server.go) | How often the server checks the host |
| `hostQuietWithStall` | 500 ms | server.go | Heartbeat silence that, with stalled viewers, means failed |
| `hostSilentTimeout` | 1.5 s | server.go | Heartbeat silence that alone means failed |
| `hostNotDelivering` | 1.5 s | server.go | Viewers stalled while the host receives fine |
| `statusFresh` | 1 s | server.go | Older statuses are ignored |
| `suspectFor` | 30 s | server.go | A failed host can't be re-elected for this long |
| `pingInterval` / `pongTimeout` | 2 s / 10 s | server.go | WebSocket cleanup only (not failure detection) |
| ICE timeouts | 3 s / 6 s / 1 s | [media.go](internal/peer/media.go) | Disconnected / failed / keepalive |

Lowering `stallAfter` and `hostQuietWithStall` makes failover faster but raises
the risk of false alarms on jittery Wi-Fi.

---

## Logs and diagnostics

All logs go to stderr with millisecond timestamps.

**Topology changes:**

```
topology v5: I am SUPERNODE; host=p3 backup=p1 degraded=false (keeping current host (50/21 Mbps))
```

Role names: `STREAMER sending to every viewer`, `STREAMER (sending to supernode)`,
`SUPERNODE`, `BACKUP supernode (warm standby)`, `viewer`.

**Link state:**

```
send → p4: connected      a link where this peer sends
send → p3: LIVE           streamer switched this link from standby to live
recv ← p2: connected      a link where this peer receives
re-offering to p4         a failed link is being rebuilt
```

**Server:**

```
[friday] p4 joined as viewer ("ana", up=450 Mbps, wired=true, canHost=true)
[friday] epoch 2: host="p4" backup="p6" (supernode p4 (450/31 Mbps))
[friday] host p4 declared failed: 2/2 viewers stalled and no heartbeat for 516ms
```

**Resource use (`-stats`):** off by default. For diagnosis, add `-stats 5s` to any
command:

```
stats: cpu 15.8% of 1 core (1.6% of 10) | peak rss 49.5 MB, go sys 41.3 MB, heap 20.4 MB | goroutines 249
```

- `cpu`: share of one core since the previous line (100% = one core fully busy).
- `rss`: memory as seen by the OS. Current and peak on Linux and Windows; peak
  only on macOS (current needs cgo there).
- `go sys` / `heap`: memory the Go runtime got from the OS / live heap objects.

Measured on an Apple Silicon Mac (10 cores) with an 8.7 Mbps stream and 8
viewers, everything on one machine:

| Role | CPU (share of one core) | Memory (peak RSS) |
|---|---|---|
| Signaling server | 0.5% | 13 MB |
| Streamer (gamecast only; the encoder is extra) | 6% | 22 MB |
| **Supernode** (forwarding to 7 viewers) | **16%** | 50 MB |
| Backup (standby) | 9% | 25 MB |
| Viewer | 8% | 20 MB |

---

## Testing

```sh
go test -race ./...                      # everything (about 15 s)
go test -race -run Hangs -v ./internal/peer/   # one scenario, with logs
GOOS=windows go vet ./...                # check the Windows build
```

| Test | What it checks |
|---|---|
| `TestRequiredUpload`, `TestElect` | Upload formula; every election rule, including opt-in, wired preference, keeping the current host and degraded mode |
| `TestElectPair` | Backup selection; the warm backup is promoted after a hang or leave; the streamer as fallback backup; no backup when the streamer hosts |
| `TestFailoverWhenSupernodeHangs` | A frozen supernode (sockets open, nothing sent) is replaced within 1.5 s (measured ~570 ms); the new supernode's own player also recovers |
| `TestFailoverWhenSupernodeLeaves` | A clean leave recovers within 500 ms (measured ~20 ms) |
| `TestStreamerPauseDoesNotTriggerFailover` | A 3 s streamer stall doesn't change the host; the stream resumes through the same host |
| `TestRTPAudioSourceReceivesOpusAndSkipsJunk` | The UDP audio input accepts Opus RTP and ignores non-RTP and RTCP datagrams |
| `TestAudioSinkWritesOggOpus` | Viewer audio output is a valid Ogg/Opus stream containing every packet |
| Audio in the integration tests | Every viewer receives game audio through the supernode; after a supernode hang, audio resumes through the backup |
| `TestVoiceChatEchoCancellationEndToEnd` | Real sidecars in two viewers: B hears A; with B's speakers leaking into its mic, A's echo comes back ≥ 20 dB quieter with echo cancellation than without (measured 64 dB); muting A silences it at B |
| `TestVoiceSidecarGetsGameAudio` | Game audio is forwarded to the sidecar (≥ 50 packets in 2 s) |
| Rust: `cargo test` in `voice/` | RTP parsing; Opus round trip; jitter buffer ordering, wrap-around, loss concealment, late packets, latency cap; resampling; mixing and mute; echo cancellation in a simulated room (measured 76 dB reduction) |
| `TestSampleSeesCPUAndMemory` | `-stats` reports CPU under load, drops when idle, and reports memory |

The integration tests run a real signaling server and real WebRTC peers in one
process over loopback, with a synthetic H.264 source and a synthetic Opus source (valid TOC byte,
20 ms packets). The H.264 source has valid NAL framing with
SPS/PPS/IDR and P-frames, and keyframes larger than the MTU so fragmentation is
exercised. They check the bytes that reach each viewer's video and audio outputs. The
voice tests run the real `gamecast-voice` binary (from `voice/target/debug`, or
`GAMECAST_VOICE_BIN`) and are skipped if it hasn't been built. They measure
tones in the recorded playback with a Goertzel filter.

---

## Project layout

```
gamecast/
├── cmd/gamecast/main.go            CLI: signal / stream / watch, flags, shutdown
└── internal/
    ├── proto/proto.go              JSON wire messages
    ├── signaling/
    │   ├── election.go             Host + backup election (pure functions)
    │   ├── election_test.go
    │   └── server.go               Rooms, health monitor, topology broadcast, SDP relay, /speedtest
    ├── peer/
    │   ├── client.go               Signaling loop, status heartbeat, topology → desired links, frame pump
    │   ├── links.go                Persistent links: offer/answer, live/standby swap, forwarding, PLI
    │   ├── audio.go                Opus codec, UDP RTP audio input, Ogg/Opus audio output
    │   ├── audio_test.go
    │   ├── media.go                WebRTC API setup (codec, interceptors, ICE), keyframe throttle
    │   ├── source.go               Annex-B H.264 → frames; FrameSource interface
    │   ├── speedtest.go            Upload measurement against the signaling server
    │   └── integration_test.go     End-to-end and failover tests
    │   ├── voice.go                Voice: sidecar process, voice mesh links, mute
    │   └── voice_test.go           End-to-end voice tests with the real sidecar
    └── procstats/                  Process CPU / memory / goroutine sampling (Unix + Windows)

voice/                              gamecast-voice: the Rust voice sidecar
├── Cargo.toml                      sonora, cpal, opusic-sys (libopus), ringbuf
├── build.rs                        Finds the system libopus via pkg-config
└── src/
    ├── main.rs                     Arguments, READY handshake, stdin commands, 10 ms loop
    ├── engine.rs                   Mixing, echo reference, sonora processing, Opus encode, RTP
    ├── jitter.rs                   Per-speaker jitter buffer with loss concealment
    ├── io.rs                       cpal devices, synthetic inputs, raw/null outputs
    ├── resample.rs                 Linear resampler, channel mapping
    ├── opus.rs                     Safe libopus wrapper
    └── rtp.rs                      Minimal RTP parse/build
```

**Extension points:**

- `peer.FrameSource` (`NextFrame`, `RequestKeyframe`): plug in a native
  capture + hardware encoder here.
- `peer.AudioSource` (`NextPacket`): plug in native system-audio capture + an
  Opus encoder here.
- `Client.OnTopology(func(proto.Topology))`: hook for a UI that shows who is
  hosting, backing up and degraded.

---

## Design decisions

**Why a supernode, and not mesh, an SFU or a CDN?**

| Option | Why not (for this use case) |
|---|---|
| Mesh (streamer sends to everyone) | 1080p60 to 9 viewers needs about 94 Mbps of upload; most streamers don't have it. gamecast still uses this when the streamer qualifies. |
| SFU (media server) | Works well, but needs a server and bandwidth bills (about 30 GB per hour for 9 viewers). The supernode is an SFU that runs on a viewer's PC instead. |
| CDN / HLS | Adds 2–30 s of delay; too slow for friends watching together over voice. |
| Relay tree (several viewers relay) | More failure points and more hops for similar savings. The supernode is a one-level tree. |

**Why H.264?** Every GPU from the last decade encodes and decodes it in
hardware. AV1 would need about 35% less bandwidth but needs recent GPUs (RTX 40,
RX 7000, Arc), and Apple Silicon has no AV1 hardware encoder.

**Why 1080p60 at 8 Mbps?** It fits on a fiber supernode for 9 viewers, keeps
game motion smooth, and 60 fps matches the most common monitor refresh rate.

**Why warm standby instead of reconnecting on failure?** Over the internet,
setting up new links (ICE, DTLS, signaling round trips) takes 300–600 ms. With
pre-connected standby links, failover is a local track swap.

**Why detect failures from viewers' stalls, not only heartbeats?** Heartbeats
over TCP can be delayed by retransmissions on a bad link, so heartbeats alone
need a long timeout to avoid false alarms. Combining "the host went quiet" with
"most of its viewers stopped getting video" lets the server act after 500 ms
safely. The host's own status rules out streamer-side stalls.

**Why Go and Pion?** Pion is a mature, pure-Go WebRTC stack: no cgo, easy
cross-compilation, and packet-level control for forwarding. A Rust rewrite would
save roughly 0.1 of a core on the supernode, which nobody would notice. Rust
becomes attractive when the native capture, encoding and audio work starts,
because those use OS and C APIs that Rust can call more easily than Go.

**Why a Rust sidecar for voice?** Voice chat needs echo cancellation, and the
only production-grade canceller (WebRTC's) is C++. From Go, that means cgo and
cross-compiling C++ for every platform. sonora is a pure-Rust port, and cpal
covers audio devices on every OS, so the sidecar avoids C++ entirely. Go keeps
doing what it does well: networking, with one WebRTC stack. The two processes
talk over local UDP, which works the same on every OS.

**Why a voice mesh instead of the supernode?** Voice is tiny (~32 kbps per
stream), so the mesh costs nothing, and it keeps voice working while the video
supernode fails over.

**Why non-trickle ICE?** Simpler signaling. Its cost (gathering time) only
applies when links are first set up, never during failover.

---

## Limitations

- **Video comes from an ffmpeg pipe.** Keyframes can't be requested on demand, and
  parsing the pipe adds about one frame of delay.
- **Voice has only been tested with synthetic audio.** Real microphones,
  speakers and room acoustics are untested; expect 20–40 dB of echo reduction in
  real rooms, not the 64–76 dB measured with simulated echo.
- **Voice uses the default devices only;** no device selection, per-speaker
  volume or push-to-talk yet.
- **Voice loss recovery is concealment only.** Opus in-band FEC is sent but not
  yet used on receive.
- **The sidecar links the system libopus by default,** so the target machine
  needs libopus installed unless it's built with `--features bundled`.
- **Audio and video are separate outputs.** With two ffplay windows there is no
  lip sync. The planned in-app player will sync them.
- **System audio capture through ffmpeg needs a loopback device** (see
  [Requirements](#requirements)).
- **Audio has a gap during failover** equal to the failover time (~0.5–0.8 s).
  Lost audio isn't recovered.
- **Playback uses ffplay**; there is no in-app player and no UI.
- **Fixed bitrate.** A viewer on a weak connection stutters instead of getting
  lower quality. There is no bitrate adaptation or SVC yet.
- **Upload is measured to the signaling server**, not between peers. Use
  `-upload` to set it manually when the measurement is misleading.
- **One streamer per room.**
- **Freeze detection needs about 500 ms.** It can't go much lower without false
  alarms on Wi-Fi.
- **No reconnection.** If a client's WebSocket drops, the process exits.
- **The signaling server keeps rooms in memory only.**
- **Not tested between real networks or on Windows** (see [Project status](#project-status)).

## Security

This is a prototype. **Don't expose it to untrusted users yet.**

- **No authentication:** anyone who knows the server address and room name can
  join, watch, or claim the streamer role if it is free.
- **No TLS on the server itself:** run it behind a TLS-terminating proxy for `wss://`.
- **The supernode can see the video.** Media is encrypted per link (DTLS-SRTP),
  but the supernode decrypts it to forward it. End-to-end encryption (SFrame)
  would fix this.
- **Speed test endpoint:** accepts up to 256 MB per request with no rate limit.
- **Self-reported capabilities:** a client can claim any upload speed to win the
  election.

## Roadmap

| Priority | Item | Notes |
|---|---|---|
| 1 | **Real-world testing** | Real video and audio via ffmpeg, peers on different networks, TURN, Windows |
| 2 | **Native capture + GPU encoding** | Windows.Graphics.Capture / ScreenCaptureKit with NVENC / AMF / QSV / VideoToolbox behind `FrameSource`, plus WASAPI loopback / ScreenCaptureKit audio behind `AudioSource`: keyframes on request, no ffmpeg, no loopback device |
| 3 | **Voice polish** | Test with real devices; device selection, per-speaker volume, push-to-talk, in-band FEC recovery, static libopus in release builds |
| 4 | **In-app player and desktop UI** | Hardware decode, vsync-paced rendering, audio/video sync; UI built on `OnTopology` |
| 5 | **Bitrate adaptation** | From congestion feedback (TWCC/REMB); temporal SVC so weak viewers get 30 fps |
| 6 | **Security** | Room tokens, `wss://`, SFrame end-to-end encryption |
| 7 | **Efficiency** | Pooled packet buffers (removes ~6,000 allocations/s on the supernode), trickle ICE, peer-to-peer upload probe |

---

## Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| ffplay shows nothing for a while | It is waiting for a keyframe. Use a short GOP (`-g 30`). |
| ffplay shows smearing after a failover | Frames were lost before the next keyframe; it clears at the next keyframe. |
| `server: room "x" already has a streamer` | Only one `stream` per room. Use another room name. |
| Topology says `degraded=true` | Nobody has enough upload for everyone. Add a `-host` viewer with more upload, lower the bitrate, or expect stutter. |
| A `-host` viewer is never elected | Its upload is below the requirement (see the election table), or someone wired / faster qualifies. Check the server's `epoch … host=` line. |
| Links stay at `connecting` | NAT/firewall blocks direct UDP. Add a TURN server with `-ice`. |
| `upload test failed … reporting 0 Mbps` | The `/speedtest` endpoint couldn't be reached. Pass `-upload` manually. |
| Upload measured far too high | The signaling server is on the same LAN or machine. Pass `-upload` with your real ISP upload. |
| The host keeps changing | Its connection is unstable (Wi-Fi jitter). Prefer a wired `-host` viewer. A failed host is excluded for 30 s. |
| Video garbled from the start when audio is enabled | ffmpeg is printing the RTP SDP into the video pipe. Add `-sdp_file audio.sdp` to the audio output. |
| No audio on viewers | Check the streamer logged `game audio: listening for Opus RTP on …` and that ffmpeg sends to that exact address. The viewer needs `-audio-out`. |
| `audio input …: address already in use` | Another process uses the port (or port+1 is taken, which is only a warning). Pick another `-audio-in` port and match it in ffmpeg. |
| A viewer hangs at startup with `-audio-out` on a named pipe | Opening a named pipe waits for a reader. Start ffplay on the pipe first. |
| `-voice: gamecast-voice not found …` | Build the sidecar (`cargo build --release` in `voice/`) and copy it next to `gamecast`, or pass `-voice-bin`. |
| Sidecar fails to start: library not loaded / `libopus` | It links the system libopus. Install it, or build with `cargo build --release --features bundled`. |
| `voice: error: no microphone found` | No default input device. Use `-voice-input silence` to listen only. |
| Others hear themselves echo | The echo canceller needs a few seconds to adapt. Make sure game audio is played by the sidecar (`-voice`), not by a separate player, so it's in the echo reference. |
| Stream stutters for everyone | The supernode's upload is saturated. Check `-stats` and the router; enable SQM/QoS. |

## FAQ

**How many viewers can it handle?** It's designed for about 10. The limit is the
supernode's upload: each viewer adds about 10 Mbps. Beyond about 10 viewers, use
a real SFU.

**Is there voice chat?** Yes: add `-voice`. Everyone with `-voice` is in one
voice mesh, with echo cancellation, noise suppression and automatic gain
control done by the Rust sidecar.

**Can viewers watch in a browser?** Not yet. The protocol is plain WebRTC, so a
browser viewer is possible, but it would need a small web client and matching
H.264 profiles.

**Does the signaling server see the video?** No. It only relays small JSON
messages. The video goes peer to peer.

**Does the supernode see the video?** Yes; see [Security](#security).

**Why does the streamer sometimes send directly?** With one viewer, or when the
streamer has enough upload, a relay would only add latency.

**What happens if the streamer leaves?** The stream ends. Viewers stay in the
room, and when a streamer joins again the topology is rebuilt for it. This should
resume playback, but it isn't covered by a test yet.

## Contributing

- Format and check: `gofmt -l .` (no output), `go vet ./...`, and
  `GOOS=windows go vet ./...`
- Tests must pass with the race detector: `go test -race ./...`
- Sidecar: `cargo fmt`, `cargo clippy --all-targets` (no warnings), `cargo test`
- Commit messages follow `type(scope): summary`, for example
  `feat(peer): …`, `fix(signaling): …`, `docs: …`, `chore: …`

## License

No license has been chosen yet. Until one is added, all rights are reserved by
the author.
