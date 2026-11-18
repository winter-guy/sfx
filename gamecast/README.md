# gamecast

Prototype of the media core for a desktop game-streaming app: **H.264 1080p60
to a small group (~10 people), with a viewer's PC acting as the forwarding
server ("supernode")** when the streamer's own upload is too small.

```
               ┌─────────────────────┐
               │  signaling server   │  rooms, election, SDP relay
               │  (no media)         │  heartbeats every 2s
               └──────────┬──────────┘
                          │ WebSocket
     ┌────────────────────┼──────────────────────┐
     │                    │                      │
 Streamer ──1 stream──▶ Supernode ──N-1 copies──▶ other viewers
 (encode once)          (a viewer on fiber:
                         forwards RTP packets,
                         no decode / re-encode)
```

Every client contains the same forwarding code, so any of them can be elected.

## Election rules

`required upload = 1.3 × bitrate × copies to send`
(a viewer-host sends one fewer copy because it is also a viewer)

0. One viewer → streamer sends directly (a relay saves nothing).
1. Keep the current host while it still qualifies (avoids churn).
2. Streamer, if it can serve everyone itself (one hop less latency).
3. Best opted-in viewer (`-host`) that qualifies: wired first, then upload, then join order.
4. Nobody qualifies → **degraded**: whoever has the most spare upload.

For 9 viewers at 8 Mbps: streamer-as-host needs 94 Mbps, viewer-as-host needs 83 Mbps.

A **backup** is elected with the same rules (excluding the host). When no
viewer qualifies, the streamer is the backup and holds standby links to every
viewer, so it can fall back to sending directly.

## Failover

The backup is a **warm standby**: links streamer → backup and backup → every
viewer are connected in advance but carry no media (the streamer's link uses
an idle track). Failover is a track swap on the streamer, with no new handshakes.

Every client sends a status heartbeat every 250ms: who it plays from and whether
packets are flowing (stalled = nothing for 400ms). The server declares the host
failed when:

| Condition | Typical case |
|---|---|
| WebSocket closed | Process exits or crashes (OS resets the connection) |
| Most of its viewers stalled **and** no heartbeat for 500ms | Frozen process, network loss, power cut |
| No heartbeat for 1.5s | Same, when no viewer is reporting |
| Host receiving fine but most viewers stalled for 1.5s | Host's upload saturated or path broken |

If the host itself reports that it isn't receiving, the streamer is the
problem (game paused, encoder stuck). The server does nothing, because
switching hosts wouldn't help. A failed host can't be re-elected for 30s.

Measured (local machine, `go test -race`):

| Event | Picture back on viewers |
|---|---|
| Supernode leaves / crashes | ~20 ms |
| Supernode hangs (no TCP reset) | ~570 ms |
| Streamer paused 3s | no failover (correct) |

Over the internet, add one signaling round trip (~20–100 ms). With the piped
ffmpeg encoder, the picture can't come back until the next scheduled keyframe
(`-g`), because a pipe can't ask the encoder for one. Use `-g 30` (0.5s) until
the native encoder lands; it can produce a keyframe on request.

## Build and test

```sh
go build -o bin/gamecast ./cmd/gamecast
go test -race ./...
```

`internal/peer/integration_test.go` runs a server, a streamer and three viewers
on the local machine. It checks that the stream flows through the elected
supernode, then covers three cases: the supernode hangs, the supernode leaves,
and the streamer pauses (which must not trigger a failover).

## Run

```sh
# 1. Signaling server (any small VPS, or one of the PCs)
gamecast signal -addr :8080

# 2. Streamer: pipe in Annex-B H.264 from a hardware encoder
#    macOS (screen index from: ffmpeg -f avfoundation -list_devices true -i "")
ffmpeg -f avfoundation -framerate 60 -capture_cursor 1 -i "1:none" \
  -vf scale=1920:1080 -c:v h264_videotoolbox -realtime 1 \
  -b:v 8M -maxrate 12M -bf 0 -g 30 -f h264 - \
| gamecast stream -server ws://SIGNAL:8080/ws -room friday

#    Windows + NVIDIA (ffmpeg 6+; AMD: h264_amf, Intel: h264_qsv)
ffmpeg -f lavfi -i ddagrab=framerate=60 -c:v h264_nvenc -preset p1 -tune ull \
  -rc cbr -b:v 8M -maxrate 12M -bufsize 8M -bf 0 -g 30 -f h264 - \
| gamecast stream -server ws://SIGNAL:8080/ws -room friday

# 3. Viewer on fiber that volunteers as supernode
gamecast watch -server ws://SIGNAL:8080/ws -room friday -host -wired -out - \
| ffplay -fflags nobuffer -flags low_delay -framedrop -f h264 -

# 4. Everyone else
gamecast watch -server ws://SIGNAL:8080/ws -room friday -out - | ffplay -fflags nobuffer -flags low_delay -framedrop -f h264 -
```

Useful flags: `-upload N` (Mbps; 0 = measure against the signaling server),
`-wired`, `-ice stun:…,turn:user:pass@host:3478`, `-bitrate 8000`,
`-in file.h264 -loop` (replay a file instead of a live encoder).

Encoder settings matter: **no B-frames** (`-bf 0`), and a short GOP (`-g 30`).
A piped encoder can't be asked for a keyframe on demand, so the GOP length
caps how fast new viewers and failovers get a picture.

## Layout

| Path | What |
|---|---|
| `internal/proto` | JSON wire messages |
| `internal/signaling/election.go` | Host + backup election (pure functions, unit-tested) |
| `internal/signaling/server.go` | Rooms, health monitor, topology broadcast, SDP relay, `/speedtest` |
| `internal/peer/client.go` | Client: signaling, status heartbeat, topology → desired links, encode-once frame pump |
| `internal/peer/links.go` | Persistent links: offer/answer, live/standby track swap, RTP forwarding, PLI→keyframe |
| `internal/peer/source.go` | Annex-B → frames; `FrameSource` is where a native encoder plugs in |
| `cmd/gamecast` | CLI |

## Not done yet

| Gap | Plan |
|---|---|
| Video comes from an ffmpeg pipe | Native capture (Windows.Graphics.Capture / ScreenCaptureKit) + NVENC/AMF/QSV/VideoToolbox behind `FrameSource`. That enables instant keyframes and removes the ~1-frame delay from parsing the pipe |
| No audio | Opus track; WASAPI loopback / ScreenCaptureKit audio |
| Playback via ffplay | In-app hardware decode with vsync-paced rendering |
| Upload is measured to the signaling server | Peer-to-peer throughput probe |
| Keyframe after failover waits for the GOP (piped encoder) | Native encoder: keyframe on request |
| Frozen-host detection needs viewers to notice the stall (~0.5s) | Can't go much lower without false alarms on Wi-Fi jitter |
| Fixed bitrate | Bitrate adaptation from TWCC/REMB feedback; temporal SVC for weak viewers |
| Supernode can see the video | SFrame end-to-end encryption |
| Anyone who knows the room name can join, no TLS | Room tokens, `wss://` |
| SDP sent only after all ICE candidates are gathered | Trickle ICE for faster connection setup |
