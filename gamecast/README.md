# gamecast

**Low-latency game streaming to a small group, with no media server.**

gamecast streams a game from one PC to a small group of friends (about 10 people)
as H.264 1080p60 over WebRTC. A streamer's home connection usually can't send 9 copies of a 1080p60
stream, so gamecast elects a **supernode**: one viewer on a fast connection
receives the stream once and forwards it to everyone else. Any client can be
elected, because every client runs the same forwarding code. A **warm backup** takes over in
about half a second if the supernode freezes or disappears.

The only server is a lightweight **signaling server** that coordinates peers.
It never touches video.

> **Status: working prototype.** The media core (election, forwarding, failover)
> is implemented and covered by automated tests. Capture and playback currently
> rely on `ffmpeg`/`ffplay`, there is no audio yet, and it has not yet been tested
> between machines on different networks. See [Project status](#project-status).

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
| Resource diagnostics (`-stats`) | ✅ Done | Unit test, real binaries |
| Real video (ffmpeg capture → ffplay) | ⚠️ Untested | ffmpeg was not available on the development machine; tests use synthetic H.264 |
| Peers on different networks (NAT, TURN) | ⚠️ Untested | Only tested over loopback |
| Windows | ⚠️ Compiles only | Passes `go vet` for Windows, never run there |
| Audio, native capture/encoding, in-app player, UI | ❌ Not started | See [Roadmap](#roadmap) |

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
| Streamer | `gamecast stream` | Reads encoded H.264 frames (currently from an ffmpeg pipe), packetizes them once and sends them on its live links. |
| Viewer | `gamecast watch` | Receives the stream and writes it out for playback (currently to ffplay). It may be elected supernode or backup. |

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

All peers negotiate exactly one codec: H.264, packetization mode 1, payload type
102. Both ends are always gamecast, so the SDP profile string is pinned. The
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

---

## Requirements

### To build

- Go **1.24** or newer
- No cgo, no C libraries

### To run (for now)

- **ffmpeg** on the streamer, to capture and hardware-encode the screen
- **ffplay** (part of ffmpeg) on viewers, to display the stream

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
go test -race ./...
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

To see failover, freeze the supernode with `kill -STOP <pid>`. The plain viewer's
picture should continue through the backup, and the server log shows
`host … declared failed`.

### A real session

1. **Run the signaling server** somewhere everyone can reach: a small VPS, or one
   of the PCs with port 8080 forwarded.

   ```sh
   gamecast signal -addr :8080
   ```

2. **Streamer:** pipe a hardware-encoded capture into gamecast (see
   [Encoder settings](#encoder-settings) for other GPUs):

   ```sh
   # macOS (find the screen index with: ffmpeg -f avfoundation -list_devices true -i "")
   ffmpeg -f avfoundation -framerate 60 -capture_cursor 1 -i "1:none" \
     -vf scale=1920:1080 -c:v h264_videotoolbox -realtime 1 \
     -b:v 8M -maxrate 12M -bf 0 -g 30 -f h264 - \
   | gamecast stream -server ws://SIGNAL_HOST:8080/ws -room friday
   ```

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
| `-host` | `false` | watch | Volunteer this PC as supernode/backup. |
| `-out` | `-` | watch | Where to write received H.264: `-` for stdout, a file path, or `""` to discard. |

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
| `TestSampleSeesCPUAndMemory` | `-stats` reports CPU under load, drops when idle, and reports memory |

The integration tests run a real signaling server and real WebRTC peers in one
process over loopback, with a synthetic H.264 source: valid NAL framing with
SPS/PPS/IDR and P-frames, and keyframes larger than the MTU so fragmentation is
exercised. They check the bytes that reach each viewer's output.

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
    │   ├── media.go                WebRTC API setup (codec, interceptors, ICE), keyframe throttle
    │   ├── source.go               Annex-B H.264 → frames; FrameSource interface
    │   ├── speedtest.go            Upload measurement against the signaling server
    │   └── integration_test.go     End-to-end and failover tests
    └── procstats/                  Process CPU / memory / goroutine sampling (Unix + Windows)
```

**Extension points:**

- `peer.FrameSource` (`NextFrame`, `RequestKeyframe`): plug in a native
  capture + hardware encoder here.
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

**Why non-trickle ICE?** Simpler signaling. Its cost (gathering time) only
applies when links are first set up, never during failover.

---

## Limitations

- **Video comes from an ffmpeg pipe.** Keyframes can't be requested on demand, and
  parsing the pipe adds about one frame of delay.
- **No audio** (neither game audio nor voice chat).
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
| 1 | **Game audio** | An Opus track on the same links; the supernode forwards it unchanged and failover covers it automatically |
| 2 | **Real-world testing** | Real video via ffmpeg, peers on different networks, TURN, Windows |
| 3 | **Native capture + GPU encoding** | Windows.Graphics.Capture / ScreenCaptureKit with NVENC / AMF / QSV / VideoToolbox behind `FrameSource`: keyframes on request, no ffmpeg |
| 4 | **Voice chat** | Peer-to-peer mesh (~0.3 Mbps per person); needs echo cancellation (OS voice-processing modes or the WebRTC audio processing module) |
| 5 | **In-app player and desktop UI** | Hardware decode, vsync-paced rendering; UI built on `OnTopology` |
| 6 | **Bitrate adaptation** | From congestion feedback (TWCC/REMB); temporal SVC so weak viewers get 30 fps |
| 7 | **Security** | Room tokens, `wss://`, SFrame end-to-end encryption |
| 8 | **Efficiency** | Pooled packet buffers (removes ~6,000 allocations/s on the supernode), trickle ICE, peer-to-peer upload probe |

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
| Stream stutters for everyone | The supernode's upload is saturated. Check `-stats` and the router; enable SQM/QoS. |

## FAQ

**How many viewers can it handle?** It's designed for about 10. The limit is the
supernode's upload: each viewer adds about 10 Mbps. Beyond about 10 viewers, use
a real SFU.

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
- Commit messages follow `type(scope): summary`, for example
  `feat(peer): …`, `fix(signaling): …`, `docs: …`, `chore: …`

## License

No license has been chosen yet. Until one is added, all rights are reserved by
the author.
