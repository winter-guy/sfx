# cast agent protocol: JSON-RPC 2.0 over stdio

**Status:** draft v0.5, not implemented yet. This spec defines the control
protocol of `cast agent`: how an **app** (a desktop UI, a bot, a test harness)
drives the **core** (`cast` plus its `voice` sidecar) as a child process,
sending JSON-RPC requests on stdin and receiving responses and events on
stdout. [Changes required](#changes-required-in-the-current-code)
lists what the code needs before the API can exist.

**v0.5** applies the decisions of
[plans/review-resolutions.md](../plans/review-resolutions.md): D11–D14, a
server grace period and resume token for reconnects, non-blocking output,
voice settings at any time, deafen, degraded tiers, and smaller fixes.

---

## Contents

- [Decisions](#decisions)
- [Principles](#principles)
- [Transport](#transport)
- [Message format](#message-format)
- [Handshake and versioning](#handshake-and-versioning)
- [Concepts and identifiers](#concepts-and-identifiers)
- [Streams and watching](#streams-and-watching)
- [Capacity](#capacity)
- [Microphone control](#microphone-control)
- [Methods](#methods)
- [Events](#events)
- [Data types](#data-types)
- [Errors](#errors)
- [Behaviour guarantees](#behaviour-guarantees)
- [Example session](#example-session)
- [Core vs app responsibilities](#core-vs-app-responsibilities)
- [Changes required in the current code](#changes-required-in-the-current-code)
- [Open questions](#open-questions)

---

## Decisions

| # | Decision | Where |
|---|---|---|
| D1 | **stdio is the only transport.** The message handling is written against a generic stream so it isn't tied to stdio internally, but no other transport is offered. | [Transport](#transport) |
| D2 | **The mic is controlled only by the user.** The core never mutes or unmutes on its own: not on a hang, a failure, a missed message or joining a server. **Push-to-talk is the mode on first install**; when the user changes it, the app remembers the choice and uses it from then on. | [Microphone control](#microphone-control) |
| D3 | **No chat for now.** Out of scope for this version of the API. | — |
| D4 | **Streaming video has its own controls, separate from voice and from room membership.** Going live, pausing, sending game audio, and what a viewer receives are all independent controls. | [Stream methods](#stream) |
| D5 | **Several streams per room**, each with its own supernode tree (host, backup, failover). Starting or stopping one stream never affects another. The number of streams is capped by the room setting **`maxStreams`, default 4**. | [Streams and watching](#streams-and-watching), [Capacity](#capacity) |
| D6 | **Watching is explicit and one stream at a time.** Joining a room watches nothing. Watching a stream leaves the one watched before. | [Streams and watching](#streams-and-watching) |
| D7 | **Streamers are the last choice as supernode**, for their own stream and for others. A watcher with enough upload hosts first, so the streamer's upload and CPU stay with the game. | [Streams and watching](#streams-and-watching) |
| D8 | **No watching while streaming** (v1). Starting a stream stops watching. | [Open questions](#open-questions) |
| D9 | **Voice doesn't depend on streams.** Starting, stopping, watching, switching or leaving a stream never interrupts voice. Voice ends only when the participant turns it off or leaves the room. | [Behaviour guarantees](#behaviour-guarantees) |
| D10 | **One core process serves one session** (one room). The app runs one core at a time. To change rooms, `session.leave` then `session.join`. | [Transport](#transport) |
| D11 | **Rooms hold at most 10 members**, set by the server setting **`maxMembers`, default 10**. A join beyond it fails with `room_full`. Every capacity figure in this spec assumes it. | [Capacity](#capacity) |
| D12 | **Hide my IP.** A member can join with `network.hideIp`: its connections use TURN relay candidates only, so peers see the TURN server's address, not its own. It applies at join, never mid-session. The signaling server can run the TURN server itself (`cast signal -turn`). | [Session](#session) |
| D13 | **Relaying for others is opt-in and off by default.** The app asks when a stream the member watches is short of relays, showing what it costs (upload and GB per hour), and remembers the answer. | [Streams and watching](#streams-and-watching) |
| D14 | **When the room's upload falls short, the extra watchers wait.** The election first lowers its safety margin from 1.3 to 1.1; if that still doesn't fit, it serves as many watchers as fit and the rest wait with `noCapacity`. Relays are never given more copies than they can carry, and the streamer is never given more than its budget. | [Streams and watching](#streams-and-watching) |

---

## Principles

1. **The core provides mechanisms and facts; the app decides policy.** The core
   can switch the mic instantly, compute speaking levels and switch devices. The
   app decides *when* to transmit (button, hotkey), how levels are drawn, and
   what to remember between runs.
2. **Anything that must behave the same in every app is core.** That includes
   reconnecting, jitter buffering, echo cancellation, permission enforcement and
   encryption.
3. **The user stays in control.** The core changes mic state only when the app
   asks it to. Nothing in the core flips mute or mode by itself.
4. **The app never handles media.** Audio and video stay inside the core. The
   API carries only commands, state and small payloads.
5. **State plus events.** The app can always fetch a full snapshot
   (`session.get`) and then apply events. If it misses events, it fetches a new
   snapshot.
6. **Additive evolution.** New methods, events and fields can appear in minor
   versions. Apps must ignore fields and events they don't recognise.

---

## Transport

The app **starts the core as a child process** and talks to it over its
standard streams, the same pattern `cast` already uses with the `voice` sidecar:

```
app ──stdin──▶  cast agent   ──stdin / UDP──▶  voice
app ◀─stdout──  (core)
       stderr ─▶ logs (free text, for the app's log file)
```

- Start with `cast agent`. This is a new command; `signal`, `stream` and
  `watch` stay as they are.
- **stdin/stdout:** UTF-8, one JSON message per line (newline-delimited JSON).
  stdout carries protocol messages only.
- **stderr:** human-readable logs. Not part of the API.
- **Lifetime:** the core exits when stdin closes, so it never outlives the app.
  When the app exits or is force-closed, the core leaves the room and stops,
  and with it the mic and the sidecar. `core.shutdown` does the same cleanly.
- **One core process = one session** (D10). Two sessions would each claim the
  full upload in supernode elections and would fight over the mic, the speakers
  and echo cancellation, so the app runs only one core at a time. If several
  sessions are ever needed, they belong in one process, and an optional
  `session` field can be added to methods and events without breaking apps.
- **Only the app that started the core can control it.** There's no network
  listener: no other local process and no website can reach the API.

**Output never blocks the core.** A slow or hung app must not stall voice or
video (D2 relies on it), so:

1. One writer owns stdout; everything else queues and never waits.
2. **Responses are never dropped.** When more than 256 responses are queued,
   the core stops reading stdin until the app catches up. The backpressure
   lands on the app, not on media.
3. **`voice.levels` keeps only the latest batch**, overwritten until written.
4. **State events** queue up to 1,000. Past that, the queued state events are
   dropped and the core writes one **`core.resync`** event; the app calls
   `session.get`.
5. **`seq` is assigned when an event is written**, so merging and dropping
   never leave a silent gap. A gap is a core bug; treat it like
   `core.resync`.
6. **stderr** is buffered the same way; dropped lines are counted in the next
   line written.

> **Consequence:** in agent mode, stdin carries control messages, so the
> streamer's video can't come from stdin as it does with `cast stream -in -`.
> See `VideoSource` in [Data types](#data-types).

---

## Message format

Messages follow **JSON-RPC 2.0** (one object per line), so existing client
libraries can be used.

**Request** (app → core):

```json
{"jsonrpc":"2.0","id":7,"method":"voice.setMode","params":{"mode":"open"}}
```

**Response** (core → app), exactly one per request, possibly out of order:

```json
{"jsonrpc":"2.0","id":7,"result":{}}
{"jsonrpc":"2.0","id":7,"error":{"code":-32010,"message":"not in a session","data":{"type":"not_in_session"}}}
```

**Event** (core → app), a JSON-RPC notification with no `id`. Every event has a
`seq` that increases by 1 as events are written (see
[Transport](#transport)):

```json
{"jsonrpc":"2.0","method":"peer.joined","params":{"seq":42,"peer":{ "clientId":"…", "name":"ana", "…":"…" }}}
```

Conventions:

| Kind | Convention |
|---|---|
| Names | Methods and events are `area.camelCase`; fields are `camelCase` |
| Time | Durations in **milliseconds** (`…Ms`); timestamps as RFC 3339 strings in UTC |
| Bitrates | **kbps** (`…Kbps`) |
| Volume | Linear gain: `1.0` = unchanged, `0` = silent, maximum `2.0`. Named `volume` everywhere |
| Levels | dBFS, from `-127` (silence) to `0` (full scale) |
| Absent field in a request | The default, or unchanged for setters |
| `null` in a request | Reset to the default (e.g. `micDelayMs: null`) |
| State (events, results, snapshots) | Every field is present; `null` means "none" (e.g. `watching: null`) |

---

## Handshake and versioning

1. The core starts and immediately emits **`core.ready`**:

   ```json
   {"jsonrpc":"2.0","method":"core.ready","params":{"seq":1,"apiVersion":"0.5","coreVersion":"0.5.0",
     "capabilities":["stream","watch","gameAudio","voice"]}}
   ```

   `capabilities` lists what this build supports. For example, `voice` is
   missing if the sidecar isn't installed.

2. The app calls **`core.hello`** before anything else:

   ```json
   → {"jsonrpc":"2.0","id":1,"method":"core.hello","params":{"apiVersion":"0.5","client":"cast-desktop/1.2.0"}}
   ← {"jsonrpc":"2.0","id":1,"result":{"apiVersion":"0.5"}}
   ```

- **Versions** are `major.minor`.
- **While the API is `0.x`**, any minor version may break compatibility, so
  `core.hello` requires an exact match and fails with `version_mismatch`
  otherwise.
- **From `1.0`:** same major is compatible, and the core answers with the lower
  of the two minors and doesn't use newer features. A different major fails
  with `version_mismatch`.

---

## Concepts and identifiers

| Concept | Description |
|---|---|
| **Session** | Membership in one room on one signaling server, from `session.join` to `session.left`. It survives reconnects. One per core process (D10). |
| **`clientId`** | A stable identifier the **app** generates (UUID), stored per installation and sent on join. Peers are identified by `clientId` everywhere in this API. A `clientId` that is in the room (or `away`) can only be resumed with the server's resume token, which the core keeps in memory and never passes to the app; anyone else gets `client_id_in_use`. |
| **Away** | A peer whose signaling connection dropped. For the server's **grace period (15 s)** it keeps its identity, stream slot, watching, duties and voice, and its media links keep running. Back within 15 s: nothing changed. Later: it is removed (`peer.left`, reason `timeout`). |
| `peerId` | The server's connection ID (`p12`). It changes on every reconnect, so it's for diagnostics only. |
| **Participant** | Everyone in the room. Anyone can talk, watch one stream, and start a stream while the room is below `maxStreams`. |
| **Stream** | One participant's live video plus optional game audio. Identified by the **streamer's `clientId`**, since a participant has at most one stream. Each stream has its own supernode tree. |
| **Watching** | Per participant: **at most one** stream, chosen explicitly, plus whether to receive its **video** and its **game audio**. Someone can be in the room for voice only. |
| **Duties** | Assigned by the server per stream and changing over time: `host` (supernode) and `backup`. Held by that stream's video watchers who opted in with `canHost` (D13) and have enough upload; the streamer only as a last resort (D7). |
| **Voice** | Optional per participant, one mesh for the whole room, independent of every stream (D9). A participant is *in voice* once it has enabled voice; whether it transmits depends on the [microphone mode](#microphone-control). |

---

## Streams and watching

**Starting.** `stream.start` claims one of the room's stream slots. The server
refuses it with `stream_limit` when `maxStreams` streams are already live, and
returns the current streamers in the error data. Starting a stream stops
watching (D8).

**Watching.** `stream.watch({streamer})` joins that stream's tree and leaves
the tree of the stream watched before, if any. `stream.unwatch` leaves without
picking another. Joining a room watches nothing.

**Supernode election, per stream.** Each stream elects its own host and backup
from its own watchers:

1. **One watcher:** the streamer sends to it directly. The streamer uploads one
   copy either way, so a relay would only add a hop.
2. **The current host,** while it still qualifies (no needless switching).
3. **The best watcher** that opted in (`canHost`) and receives video: wired
   first, then most upload, then whoever joined first.
4. **The streamer,** if it can send to every watcher itself (D7: last choice).
5. Otherwise the stream is **degraded** (D14), in two tiers:
   - **`tight`:** the election runs again with a safety margin of 1.1
     instead of 1.3 (upload is measured as payload; RTP and IP headers add
     3–5% on the wire, so the margin never goes to 1.0). Everyone is served.
   - **`full`:** as many watchers as fit are served; the rest, latest to
     start watching first, get `stream.state: waiting` with reason
     `noCapacity`. `Stream` carries `unserved` and `maxBitrateKbps`, the
     highest bitrate at which everyone would fit, so the app can offer the
     streamer a lower bitrate and ask watchers to relay (D13).

   Nobody is given more copies than it can carry, and the streamer never
   sends more than it would anyway.

A watcher who only takes game audio (`video: false`) can't host, because it
doesn't receive the video. It still has a parent in the tree, which sends it
game audio only (128 kbps, not a video copy). The single-watcher rule counts
video watchers.

Making the streamer the last choice costs one extra hop of latency for viewers
(a few milliseconds on a LAN, roughly 10–40 ms over the internet), and keeps
the streamer's upload at **one copy** in exchange.

**Switching.** Moving to another stream leaves the old tree completely, so the
old host stops sending a copy. If the participant leaving was the old stream's
host, its backup takes over through the planned-leave path (about 20 ms in
tests). Joining the new tree takes a connection setup plus a keyframe (not
measured yet; expect about a second).

**Stopping.** `stream.stop`, leaving the room or timing out ends the stream.
Its watchers go back to watching nothing (`stream.state: idle`). Voice is
unaffected (D9).

---

## Capacity

These numbers explain the defaults. They aren't enforced except through
`maxStreams` and the election.

**Assumptions:** 1080p60 H.264 at 8 Mbps plus 128 kbps of game audio, with
1.3× headroom, gives **≈10.6 Mbps of upload per copy sent** and ≈8.3 Mbps of
download per watcher. Voice is Opus at 32 kbps, about 50 kbps per peer with
packet headers, so **≈0.5 Mbps up and down per participant** in a room of 10,
whatever the streams do.

**Per stream:**

| Who | Upload |
|---|---|
| Streamer | One copy, ≈10.6 Mbps, when a watcher hosts |
| Host (a watcher) | 10.6 × (watchers − 1) |
| Each watcher | Downloads ≈8.3 Mbps |

**Host upload limits the size of one tree:** 20 Mbps up can host 2 watchers,
50 Mbps can host 5, and 100 Mbps can host 10.

**A room of 10 (the cap, D11), streamers not watching (D8):**

| Streams | Watchers | Split | Biggest host needs | Each streamer | Room total upload |
|---|---|---|---|---|---|
| 1 | 9 | 9 | 85 Mbps | 10.6 | ≈ 95 Mbps |
| 2 | 8 | 4 + 4 | 32 Mbps | 10.6 | ≈ 85 Mbps |
| 3 | 7 | 3 + 2 + 2 | 21 Mbps | 10.6 | ≈ 74 Mbps |
| 4 | 6 | 2 + 2 + 1 + 1 | 10.6 Mbps | 10.6 | ≈ 64 Mbps |
| 5 | 5 | 1 each | none (direct) | 10.6 | ≈ 53 Mbps |

Voice adds about 5 Mbps across the room.

**What follows:**

- **Load depends on watchers, not streams.** Each watcher receives exactly one
  copy (D6), so the room never carries more copies than it has watchers. A
  stream nobody watches sends nothing.
- **More streams make hosting easier.** The watchers split into smaller trees.
- **The worst case is popularity, not stream count.** If every watcher picks
  the same stream, it needs the same host as a one-stream room, and the
  election handles it (watcher host, then streamer, then degraded).
- **At most ⌊participants / 2⌋ streams can have a watcher at once** (5 in a
  room of 10), because streamers don't watch.
- **Every streamer needs about 11 Mbps of upload** once someone watches.

**Why `maxStreams` defaults to 4.** The cap isn't there to protect the
network. It keeps the stream list short enough to choose from and stops a small
group from splitting into audiences of one. In a room of 10, 4 is one below the
point where every stream has a single watcher. The server never refuses a
stream because hosting looks impossible: the degraded state reports that
(D14), and it changes as people switch streams.

---

## Microphone control

The mic is controlled **only by the user, through the app**. The core changes
what it transmits only in response to these calls.

### Modes

| Mode | Transmits | Calls |
|---|---|---|
| **`ptt`** (push-to-talk) | While the key is pressed | `voice.ptt({pressed: true})` on key down, `voice.ptt({pressed: false})` on key up |
| **`open`** (open mic) | Whenever not muted | `voice.setMuted({muted})` |

- **First install:** the app starts in `ptt`.
- **After that:** when the user changes the mode, the **app** stores it and
  passes it in `session.join` from then on. The core has no persistent settings;
  it uses whatever mode the app sends, and `ptt` if none is given.
- The same applies to the open-mic mute state: the app decides what to pass in
  `session.join` (for example the user's last choice). **Joining a room doesn't
  mute**: in `open` mode with `muted: false`, the mic transmits as soon as the
  voice links connect.
- The voice settings always exist in the core, starting at the defaults, and
  every voice setter works whether or not voice is enabled. Settings made
  while voice is off apply when it's turned on.
- **Deafen** (`voice.setDeafened`) silences what we hear and doesn't touch the
  mic. An app that wants deafen to mute the mic as well calls both.

### What doesn't change the mic

The core **does not** mute or unmute because of:

- the app being slow, hanging or not sending anything for a while
- a push-to-talk key held for a long time
- joining a room, reconnecting to the signaling server, or switching devices
- starting, stopping, watching or switching streams
- a device problem: if the mic fails, the core reports it (`error` event and
  `voice.state`) and falls back to the default device where possible, but the
  mode and mute state stay as the user set them

Two consequences to be aware of:

- **If the app hangs with the push-to-talk key held,** the mic keeps
  transmitting until the app sends `voice.ptt({pressed: false})` or exits.
- **When the app exits or is force-closed,** the core's stdin closes and the core
  shuts down, so the mic stops because the process ends, not because of a mute.
  The sidecar likewise stops when `cast` ends.

---

## Methods

### Core

| Method | Params | Result | Notes |
|---|---|---|---|
| `core.hello` | `apiVersion`, `client` | `apiVersion` | Must be the first call |
| `core.shutdown` | — | `{}` | Leaves the session, stops the sidecar, then exits |

### Session

| Method | Params | Result | Notes |
|---|---|---|---|
| `session.join` | `JoinParams` | `{ self: Peer, room: Room }` | Resolves once the server has welcomed us. Fails with `already_in_session`, `room_full` (D11), `client_id_in_use`, or `turn_unavailable` (`hideIp` without a TURN server, D12) |
| `session.leave` | — | `{}` | Leaves the room (ends our stream if live); the process stays up for another `join` |
| `session.get` | — | `SessionSnapshot` | Full current state; use after a `seq` gap |

**`JoinParams`:**

```jsonc
{
  "server": "wss://signal.example.com/ws",
  "room": "friday",
  "token": "…",                       // optional: room credential (see Room)
  "clientId": "6f1c…",                // required, stable per installation
  "name": "ana",
  "network": {
    "uploadMbps": "auto",             // number, or "auto" to measure
    "wired": true,
    "canHost": true,                  // relay for the stream we watch (D13: off unless the user agreed)
    "hideIp": false,                  // D12: TURN relay candidates only; needs a TURN server
    "ice": [{"urls":["stun:stun.l.google.com:19302"]},   // optional: overrides the servers from
            {"urls":["turn:turn.example.com:3478"],"username":"u","credential":"p"}]  // the signaling server
  },
  "playback": {
    "video": {"kind":"pipe","path":"/tmp/cast-out.h264"}, // VideoSink; until the in-app player exists
    "gameVolume": 1.0
  },
  "peers": {                          // optional: this app's saved settings per participant
    "b77…": {"volume": 0.6, "muted": false}
  },
  "voice": {                          // optional; omit to join without voice
    "enabled": true,
    "mode": "ptt",                    // the user's saved mode; "ptt" if omitted
    "muted": false,                   // open mode only: the user's saved choice; joining doesn't change it
    "inputDevice": "default",         // device id or "default"
    "outputDevice": "default",
    "micDelayMs": null                // optional; null = core default (80 while streaming, else 0)
  }
}
```

`network.ice` is optional: by default the core uses the STUN and TURN servers
the signaling server hands out in its welcome, including short-lived
credentials for its own TURN server (`cast signal -turn`).

Joining watches nothing (D6); the app calls `stream.watch` once the user picks
a stream.

### Stream

Streaming video is controlled separately from voice and from room membership
(D4). See [Streams and watching](#streams-and-watching) for how the pieces fit.

**Streamer side:**

| Method | Params | Result | Notes |
|---|---|---|---|
| `stream.start` | `StreamParams` | `{ stream: Stream }` | Go live. Stops watching (D8). Fails with `stream_limit` when the room has `maxStreams` streams, or `already_streaming` |
| `stream.stop` | — | `{}` | End our stream; stay in the room and in voice |
| `stream.setPaused` | `paused` | `{}` | Keep the slot but send no video (e.g. while changing scenes). Watchers see `paused`; no failover is triggered |
| `stream.setGameAudio` | `enabled` | `{}` | Stop or resume sending game audio, without touching the video |
| `stream.update` | `bitrateKbps?`, `fps?` | `{}` | Used to size the supernode. A piped encoder must be changed separately; the native encoder (future) will apply it directly |

**`StreamParams`:**

```jsonc
{
  "video": { "source": {"kind":"pipe","path":"/tmp/cast-video"}, "bitrateKbps": 8000, "fps": 60 },
  "gameAudio": { "source": {"kind":"rtp","listen":"127.0.0.1:5004"}, "bitrateKbps": 128 }   // optional
}
```

**Viewer side:**

| Method | Params | Result | Notes |
|---|---|---|---|
| `stream.watch` | `streamer`, `video?`, `gameAudio?` (both default `true`) | `{}` | Watch this stream; leaves the one watched before. Fails with `unknown_stream`, or `watch_while_streaming` while we're live (D8) |
| `stream.unwatch` | — | `{}` | Stop watching; stay in the room and in voice |
| `stream.setWatching` | `video?`, `gameAudio?` | `{}` | Change what we receive from the stream we watch. With `video: false` the host stops sending us video, which saves its upload, and we can no longer host. Both `false` is invalid params: use `stream.unwatch` |
| `stream.setGameVolume` | `volume` | `{}` | Local playback volume of game audio; only affects this device |

### Voice

| Method | Params | Result | Notes |
|---|---|---|---|
| `voice.setEnabled` | `enabled`, `mode?`, `muted?`, `inputDevice?`, `outputDevice?`, `micDelayMs?` | `{}` | Applies any settings given, then joins or leaves the voice mesh without leaving the room. Turning voice off stops the sidecar, which closes the mic device. The only way, besides leaving the room, to end voice (D9) |
| `voice.setMode` | `mode`: `"ptt"` \| `"open"` | `{}` | Takes effect immediately for this session. **The app saves it** as the user's default for next time. Switching to `ptt` stops transmitting until the key is pressed; switching to `open` uses the current `muted` value |
| `voice.ptt` | `pressed` | `{}` | `ptt` mode: key down / key up. Applied within 20 ms |
| `voice.setMuted` | `muted` | `{}` | `open` mode: mute or unmute. Applied within 20 ms |
| `voice.setDeafened` | `deafened` | `{}` | Silence all voice playback on this device. Doesn't change the mic |
| `voice.setPeerVolume` | `clientId`, `volume` | `{}` | Local only; others are unaffected. Any `clientId`, in the room or not: the core remembers it for the session |
| `voice.setPeerMuted` | `clientId`, `muted` | `{}` | Local only; any `clientId`, as above |
| `voice.setMicDelay` | `ms` (or `null` for the default) | `{}` | Echo-cancellation tuning while streaming |

Every voice method works whether or not voice is enabled; settings made while
voice is off apply when it's turned on.

### Audio devices

| Method | Params | Result | Notes |
|---|---|---|---|
| `audio.listDevices` | — | `{ inputs: Device[], outputs: Device[] }` | Doesn't open devices |
| `audio.selectDevices` | `input?`, `output?` (id or `"default"`) | `{}` | Switches live. Doesn't change the mode or mute state |

### Room (moderation, open question)

| Method | Params | Result | Notes |
|---|---|---|---|
| `room.kick` | `clientId` | `{}` | Needs moderator permission, **enforced by the server** |
| `room.serverMute` | `clientId`, `muted` | `{}` | Would be the one case where someone other than the user stops their mic. Depends on the moderation model |
| `room.setMaxStreams` | `maxStreams` (1 or more) | `{}` | Lowering it doesn't end live streams; it only blocks new ones until the count drops |

Whether these exist, and who counts as a moderator, is still open (see
[Open questions](#open-questions)). Until then, `maxStreams` comes from the
server's configuration.

### Diagnostics

| Method | Params | Result | Notes |
|---|---|---|---|
| `diag.getStats` | — | `Stats` | Process CPU/memory, per-link RTT/loss/bitrate, jitter buffer state |

---

## Events

| Event | Params (besides `seq`) | When |
|---|---|---|
| `core.ready` | `apiVersion`, `coreVersion`, `capabilities` | Once, at startup |
| `core.resync` | `dropped` | The app fell behind and queued events were dropped; call `session.get` |
| `connection.state` | `state`, `attempt?`, `retryInMs?`, `reason?` | Signaling connection changes. `state`: `connecting`, `connected`, `reconnecting`, `disconnected` |
| `session.joined` | `self: Peer`, `room: Room` | After join (also returned by `session.join`) |
| `session.left` | `reason`, `error?` | `left` (we asked), `kicked`, `serverClosed`, `failed` (gave up reconnecting). The only signal that the session ended; `error: {type, message}` says why when it wasn't asked for |
| `room.updated` | `room: Room` | A room setting such as `maxStreams` changed |
| `peer.joined` | `peer: Peer` | Someone joined the room |
| `peer.left` | `clientId`, `reason` | Someone left (`left`, `timeout`: away longer than the grace period, `kicked`) |
| `peer.updated` | `peer: Peer` | A participant's duties, streaming, watching, voice state or connection (`away`) changed |
| `stream.started` | `stream: Stream` | Someone went live |
| `stream.updated` | `stream: Stream` | A stream paused or resumed, turned game audio on or off, changed bitrate, gained or lost watchers, changed host or backup, its streamer went away or came back, or its degraded state changed |
| `stream.ended` | `streamer`, `reason` | A stream ended: `stopped`, `left`, `timeout`, `sourceEnded` (a `file` source without `loop` reached its end) |
| `stream.state` | `state`, `streamer?`, `fromClientId?`, `reason?` | Our own playback: `idle` (not watching), `waiting`, `playing`, `paused`, `stalled`, `switching` (failover in progress). `fromClientId` is who we receive from. `waiting` with reason `noCapacity`: the room's upload can't serve us yet (D14) |
| `voice.state` | `VoiceState` | Our own voice state changed (always as a result of an app call, a moderator action, or a device change) |
| `voice.levels` | `levels: [{ clientId, levelDb, speaking }]` | **Up to 10 per second** while anyone is speaking. Includes ourselves. Batched to avoid flooding |
| `voice.link` | `clientId`, `state` | A voice connection to a participant: `connecting`, `connected`, `failed` |
| `audio.devicesChanged` | `inputs`, `outputs` | Devices were added or removed. If the selected device vanished, the core falls back to the default and reports it in `voice.state` |
| `error` | `type`, `message` | A problem not tied to a request that doesn't end the session (e.g. `source_failed`, `device_failed`). A session that ends gets `session.left`; a core that can't go on exits, closing stdout |

---

## Data types

```jsonc
// Room
{ "name": "friday", "maxStreams": 4, "maxMembers": 10 }

// Peer
{
  "clientId": "6f1c…",
  "peerId": "p12",                   // diagnostics only; changes on reconnect
  "name": "ana",
  "self": false,
  "connection": "connected",         // "connected" | "away" (signaling dropped; within the grace period)
  "streaming": false,                // has a live stream
  "watching": { "streamer": "a91…", "video": true, "gameAudio": true },   // null when not watching
  "duties": ["host"],                // [] | ["host"] | ["backup"], for the stream it watches
  "voice": { "inVoice": true },      // who is speaking: voice.levels only
  "local": { "volume": 1.0, "muted": false }   // this app's local settings for that participant
}

// Stream (one per live participant)
{
  "streamer": "a91…",                // clientId; also the stream's identifier
  "streamerAway": false,             // the streamer's signaling dropped (grace period)
  "startedAt": "2026-11-20T19:02:11Z",
  "paused": false,
  "pausedBy": null,                  // "streamer" | "source" (the video pipe ended; resumes when it reopens) | null
  "gameAudio": true,
  "bitrateKbps": 8000,
  "fps": 60,
  "watchers": ["6f1c…", "b77…"],     // clientIds watching (video or game audio)
  "host": "b77…",                    // clientId or null; the streamer itself when it sends directly
  "backup": null,                    // clientId or null
  "degraded": null,                  // null | "tight" (margin lowered to 1.1) | "full" (some watchers wait), D14
  "unserved": [],                    // clientIds waiting with noCapacity
  "maxBitrateKbps": null,            // when degraded: the bitrate at which everyone fits
  "election": { "reason": "bestWatcher" }   // direct | keptHost | bestWatcher | streamerLastResort | degraded
}

// VoiceState (our own)
{
  "enabled": true,
  "mode": "ptt",                     // "ptt" | "open"
  "pttPressed": false,               // ptt mode
  "muted": false,                    // open mode
  "deafened": false,                 // voice playback silenced on this device
  "transmitting": false,             // enabled && (ptt ? pttPressed : !muted)
  "inputDevice": "default",
  "outputDevice": "default",
  "micDelayMs": 0
}

// SessionSnapshot
{
  "seq": 120,                        // events after this seq apply on top
  "connection": "connected",
  "room": { /* Room */ },
  "self": { /* Peer */ },
  "peers": [ /* Peer */ ],
  "streams": [ /* Stream */ ],       // every live stream in the room
  "playback": { "state": "playing", "streamer": "a91…", "fromClientId": "…" },
  "voice": { /* VoiceState */ }
}

// Device
{ "id": "coreaudio:BuiltInMicrophoneDevice", "name": "MacBook Pro Microphone",
  "default": true, "channels": 1, "sampleRate": 48000 }

// VideoSink (watcher playback)
{ "kind": "pipe",   "path": "/tmp/cast-out.h264" }   // Annex-B H.264
{ "kind": "none" }                                   // audio only, or testing
{ "kind": "native" }                                 // future: in-app player

// Stats (diag.getStats): our own machine only
{
  "process": { "cpuPct": 4.1, "memMb": 52 },
  "sidecar": { "cpuPct": 2.0, "memMb": 18 },
  "links": [ { "clientId": "b77…", "kind": "video",            // "video" | "voice"
               "direction": "out", "path": "direct",           // "direct" | "relay" (TURN)
               "rttMs": 23, "lossPct": 0.1, "bitrateKbps": 8100 } ],
  "jitter": { "voiceMs": 40, "gameMs": 60 }
}

// VideoSource (streamer)
{ "kind": "pipe",   "path": "/tmp/cast-video" }      // Annex-B H.264 from a named pipe; on its end the
                                                     // stream pauses (pausedBy "source") and resumes on reopen
{ "kind": "file",   "path": "clip.h264", "loop": true }   // without loop, the stream ends (sourceEnded)
{ "kind": "native", "display": "main", "encoder": "auto" }   // future: built-in capture + GPU encoding

// AudioSource (streamer game audio)
{ "kind": "rtp",    "listen": "127.0.0.1:5004" }     // Opus RTP, e.g. from ffmpeg
{ "kind": "native" }                                 // future: system-audio capture
```

---

## Errors

`error.code` follows JSON-RPC: `-32700` parse error, `-32600` invalid request,
`-32601` unknown method, `-32602` invalid params. Application errors use
`-32010` with a machine-readable `error.data.type`:

| `type` | Meaning |
|---|---|
| `version_mismatch` | `core.hello` with an incompatible major version |
| `not_ready` | Called before `core.hello` |
| `already_in_session` / `not_in_session` | Wrong state for the call |
| `stream_limit` | `stream.start` while the room already has `maxStreams` streams. `data`: `{ maxStreams, streamers: [clientId] }` |
| `already_streaming` | `stream.start` while our stream is live |
| `not_streaming` | A streamer-side `stream.*` call while not live |
| `unknown_stream` | `stream.watch` for a participant who isn't live |
| `watch_while_streaming` | `stream.watch` while our own stream is live (D8) |
| `not_watching` | `stream.setWatching` while not watching anything |
| `wrong_mode` | `voice.ptt` in `open` mode, or `voice.setMuted` in `ptt` mode |
| `unauthorized` | Bad or missing room token |
| `forbidden` | Not allowed (e.g. kick without moderator rights) |
| `room_full` | `session.join` when the room has `maxMembers` members (D11). `data`: `{ maxMembers }` |
| `client_id_in_use` | `session.join` with a `clientId` that is already in the room or `away`, e.g. the same installation on a second PC |
| `turn_unavailable` | `session.join` with `hideIp` and no TURN server, from the signaling server or `network.ice` (D12). The core never falls back to exposing the address |
| `unknown_peer` | A `room.*` call for a `clientId` not in the room |
| `voice_unavailable` | Sidecar missing or failed to start (see stderr) |
| `device_not_found` | Unknown device id |
| `device_failed` | The OS refused the device (e.g. **microphone permission denied** on macOS) |
| `source_failed` | Video or audio source couldn't be opened |
| `server_unreachable` | Couldn't connect at join (later drops are reconnected instead) |

---

## Behaviour guarantees

**Mic.** Mic state changes only through `voice.setMode`, `voice.ptt`,
`voice.setMuted` and `voice.setEnabled` (and, if moderation is adopted,
`room.serverMute`). See [Microphone control](#microphone-control).

**Voice survives streams (D9).** Starting or stopping a stream, watching,
switching, unwatching, a stream ending, or a supernode failover never
interrupts voice and never restarts the sidecar. Voice links are separate from
every stream's links. Voice ends only through `voice.setEnabled(false)`,
`session.leave`, or the core exiting.

**Streams are independent (D5).** Each stream has its own election, host,
backup and failure detection. Something happening to one stream (it starts,
stops, fails over, or loses its host) doesn't change any other stream's tree.

**Reconnect.** Two windows, which are different on purpose:

- **The server's grace period, 15 s** (a server setting). When a peer's
  signaling connection drops, the server marks it `away` instead of removing
  it: it keeps its identity, stream slot, watching, duties and voice. Others
  see `peer.updated` (and `stream.updated` with `streamerAway`), not leave +
  join. After 15 s the peer is removed (`peer.left`, reason `timeout`) and its
  stream ends (`timeout`), so a crashed streamer never holds a slot for long.
- **The core's retry window, 2 minutes.** The core emits
  `connection.state: reconnecting` and retries with exponential backoff from
  0.5 s to 10 s, with jitter. Back within the grace period, it resumes with
  its resume token and nothing changed. Back later, it joins again with the
  same `clientId`: it stays in the session from the app's point of view, but
  its own stream has ended (`stream.ended`) and it watches nothing
  (`stream.state: idle`). After 2 minutes it gives up with
  `session.left: failed`.

Media links that survived keep running; failed ones are rebuilt. Mic mode and
mute are unchanged.

**Media keeps going.** Video and voice flow peer to peer, so a short signaling
outage doesn't interrupt them. Only topology changes wait for the reconnect.
A host whose signaling drops stays host while its viewers report video
flowing: failover is decided by what the viewers see (a hung host is still
replaced in about 570 ms), and missing heartbeats alone fail a host only when
none of its viewers is reporting.

**Ordering.** Events are emitted in `seq` order. A response may arrive before
or after the events its request caused. Use the events as the source of truth
for state.

**Events are idempotent.** Every event replaces state rather than describing a
change: `peer.updated` carries the whole `Peer`, `stream.updated` the whole
`Stream`, `peer.left` and `stream.ended` delete by id. Applying an event twice,
or after a snapshot that already contains it, is harmless. After
`session.get`, apply the events whose `seq` is above the snapshot's.

**Idempotent setters.** Calling a setter twice with the same value is fine and
emits at most one event. `stream.watch` for the stream already watched changes
only the `video` and `gameAudio` flags.

**Latency.** `voice.ptt`, `voice.setMuted` and `voice.setEnabled(false)` take
effect within 20 ms of the request being read. `voice.levels` is at most 100 ms
old.

**Local settings are local.** Mic mode and mute preference, deafen,
per-participant volume and mute, and game volume aren't sent to anyone, and
the core doesn't persist them across restarts; the app does, and passes them
in `session.join` (`voice`, `playback.gameVolume`, `peers`).

---

## Example session

`→` app to core, `←` core to app (one JSON message per line on the wire; shown
abbreviated). The user switched to open mic in an earlier run, so the app
passes `"mode":"open"`. Two participants, `a91…` and `b77…`, stream during the
session.

```
← {"method":"core.ready","params":{"seq":1,"apiVersion":"0.5","capabilities":["stream","watch","gameAudio","voice"]}}
→ {"id":1,"method":"core.hello","params":{"apiVersion":"0.5","client":"cast-desktop/1.2.0"}}
← {"id":1,"result":{"apiVersion":"0.5"}}
→ {"id":2,"method":"session.join","params":{"server":"wss://…/ws","room":"friday","clientId":"6f1c…","name":"ana",
     "network":{"uploadMbps":"auto","wired":true,"canHost":true},"voice":{"enabled":true,"mode":"open","muted":false}}}
← {"method":"connection.state","params":{"seq":2,"state":"connected"}}
← {"method":"session.joined","params":{"seq":3,"self":{"clientId":"6f1c…","watching":null,"…":"…"},"room":{"name":"friday","maxStreams":4}}}
← {"id":2,"result":{"self":{"clientId":"6f1c…","…":"…"},"room":{"name":"friday","maxStreams":4}}}
← {"method":"voice.state","params":{"seq":4,"enabled":true,"mode":"open","muted":false,"transmitting":true,"…":"…"}}

   … a91 is already live; joining watches nothing, so ana picks it …
← {"method":"stream.started","params":{"seq":5,"stream":{"streamer":"a91…","watchers":[],"host":null,"…":"…"}}}
→ {"id":3,"method":"stream.watch","params":{"streamer":"a91…"}}
← {"method":"stream.state","params":{"seq":6,"state":"waiting","streamer":"a91…"}}
← {"id":3,"result":{}}
← {"method":"stream.updated","params":{"seq":7,"stream":{"streamer":"a91…","watchers":["6f1c…"],"host":"a91…","election":{"reason":"direct"},"…":"…"}}}
← {"method":"stream.state","params":{"seq":8,"state":"playing","streamer":"a91…","fromClientId":"a91…"}}

   … b77 goes live too; a91's stream is unaffected …
← {"method":"stream.started","params":{"seq":9,"stream":{"streamer":"b77…","watchers":[],"…":"…"}}}

   … ana switches to b77: she leaves a91's tree; voice keeps running …
→ {"id":4,"method":"stream.watch","params":{"streamer":"b77…"}}
← {"method":"stream.state","params":{"seq":10,"state":"waiting","streamer":"b77…"}}
← {"id":4,"result":{}}
← {"method":"stream.updated","params":{"seq":11,"stream":{"streamer":"a91…","watchers":[],"…":"…"}}}
← {"method":"stream.updated","params":{"seq":12,"stream":{"streamer":"b77…","watchers":["6f1c…"],"host":"b77…","…":"…"}}}
← {"method":"stream.state","params":{"seq":13,"state":"playing","streamer":"b77…","fromClientId":"b77…"}}

   … the user switches back to push-to-talk; the app saves "ptt" as the new default …
→ {"id":5,"method":"voice.setMode","params":{"mode":"ptt"}}
← {"method":"voice.state","params":{"seq":14,"mode":"ptt","pttPressed":false,"transmitting":false,"…":"…"}}
← {"id":5,"result":{}}
→ {"id":6,"method":"voice.ptt","params":{"pressed":true}}               (key down)
← {"method":"voice.state","params":{"seq":15,"pttPressed":true,"transmitting":true,"…":"…"}}
← {"id":6,"result":{}}
→ {"id":7,"method":"voice.ptt","params":{"pressed":false}}              (key up)
← {"method":"voice.state","params":{"seq":16,"pttPressed":false,"transmitting":false,"…":"…"}}
← {"id":7,"result":{}}

   … b77 stops; ana is back to watching nothing, voice untouched …
← {"method":"stream.ended","params":{"seq":17,"streamer":"b77…","reason":"stopped"}}
← {"method":"stream.state","params":{"seq":18,"state":"idle"}}

   … two more people go live, making 4 streams; ana tries to go live …
← {"method":"stream.started","params":{"seq":19,"stream":{"streamer":"c03…","…":"…"}}}
← {"method":"stream.started","params":{"seq":20,"stream":{"streamer":"d48…","…":"…"}}}
← {"method":"stream.started","params":{"seq":21,"stream":{"streamer":"e5a…","…":"…"}}}
→ {"id":8,"method":"stream.start","params":{"video":{"source":{"kind":"pipe","path":"/tmp/cast-video"},"bitrateKbps":8000,"fps":60}}}
← {"id":8,"error":{"code":-32010,"message":"the room already has 4 streams","data":{"type":"stream_limit","maxStreams":4,"streamers":["a91…","c03…","d48…","e5a…"]}}}

→ {"id":9,"method":"core.shutdown"}
← {"method":"session.left","params":{"seq":22,"reason":"left"}}
← {"id":9,"result":{}}
```

---

## Core vs app responsibilities

| Feature | Core | App |
|---|---|---|
| Reconnect | All of it | Show "reconnecting…", and others as `away` |
| TURN and hiding the IP | Use the signaling server's TURN (or `network.ice`); relay-only with `hideIp`, or `turn_unavailable` | Offer "hide my IP" (on by default for anyone who has streamed before; asked on a first stream); remember it |
| Relaying for others | Enforce the cap through the tree's shape; report `degraded`, `unserved`, `maxBitrateKbps` | Off by default; ask when a watched stream is short of relays, showing upload and GB per hour (D13); remember the answer |
| **Mic mode and mute** | Apply `voice.setMode`, `voice.ptt`, `voice.setMuted` within 20 ms; never change them by itself | Start in `ptt` on first install; **save the user's mode (and open-mic mute) and pass it on every join**; global hotkey, buttons |
| Streaming video | `stream.*`: go live, pause, game audio on/off; the room's stream limit is enforced by the server | Go-live UI, source picker; show who's streaming when the limit is hit |
| Watching | One stream at a time, switching trees, per-stream election | Stream list and picker, video/game-audio toggles |
| Who is speaking | Map voice streams to `clientId`; compute levels; `voice.levels` | Draw indicators |
| Per-person volume / mute, deafen | Per-participant volume in the mixer; deafen silences playback only | Slider; remember the settings locally and pass them in `session.join` (`peers`) |
| Join / leave | `peer.*` and `stream.started` / `stream.ended` events | Notifications and sounds |
| Loss, jitter, drift, DTX | All of it, invisible to the app | — |
| Devices | List, switch, fall back when unplugged | Picker; remember the choice |
| Moderation | **Server-enforced** permissions (model still open) | Host-controls UI; getting tokens |
| Mic permission (macOS) | Opening the device triggers the prompt; `device_failed` if denied | Declare the microphone usage description in the app bundle; guide the user |
| Settings, tray, updates, accounts | — | All of it |

---

## Changes required in the current code

| # | Change | Where |
|---|---|---|
| 1 | Add a `cast agent` command: JSON-RPC over stdio, an event bus with `seq` assigned at write, the non-blocking output rules of [Transport](#transport), state snapshots | `cast/cmd/cast`, new `cast/internal/agent` package |
| 2 | ~~Move what the agent needs out of `internal/`~~ **Dropped:** Go lets `cast/internal/agent` import `cast/internal/...`; revisit only if an outside program needs the core as a library | — |
| 3 | Video input other than stdin: named pipe / file in agent mode, with pause on a pipe's end and resume on reopen. Ships with #1: without it, an agent can't go live | `peer.FrameSource` |
| 4 | **Stable `clientId`** in `join`, with a **resume token** issued in `welcome`; `client_id_in_use` without it. The server marks a dropped peer **`away`** for a 15 s grace period, keeping its identity, stream slot, watching, duties and voice; today a closed WebSocket removes the peer at once | `proto.Join`, `signaling/server.go` |
| 5 | **Signaling reconnect** with backoff, keeping media links alive. **Failover rule:** missing heartbeats alone fail a host only when none of its viewers reports fresh status (backstop: the grace period); today 1.5 s of silence fails a host even while its video flows | `peer/client.go` (today `Run` returns and the process exits), `signaling/server.go` (`checkHost`) |
| 6 | **Several streams per room:** one stream per streaming participant, each with its own election, host, backup, epoch, suspects and failure monitor. `stream.start` / `stream.stop` claim and release a slot up to `maxStreams` (server-enforced) instead of a role fixed at join. Topology becomes a list of streams. Pause, and game audio on/off. Today a room has exactly one streamer | `proto`, `signaling/server.go`, `peer/client.go` |
| 7 | **Watching:** explicit and one stream at a time, with the `video` and `gameAudio` flags in the `watch` message. The server puts a watcher in that stream's tree only; the host sends only to its watchers (game audio only to audio-only watchers); the election counts only that stream's video watchers. Switching leaves the old tree, with a planned failover if the participant was its host. Today joining a room means watching | `proto`, `signaling/server.go`, `signaling/election.go`, `peer/links.go` |
| 8 | **Election order:** watchers first, the streamer only as a last resort (D7). The single-watcher rule stays. Today the streamer is preferred right after the current host | `signaling/election.go` |
| 9 | **Live game mode in the sidecar:** a `game play\|reference\|off` stdin command (with the mic delay following the mode), so starting, stopping or switching streams never restarts the sidecar and never drops voice (D9). Today the mode is fixed by `--game` at startup | `peer/voice.go`, `voice/src/main.rs`, `engine.rs` |
| 10 | **Mic modes:** `ptt` and `open` in the sidecar (transmit gate driven only by commands from `cast`), applied within 20 ms; mode and mute passed in at start. Today the sidecar has only `--muted` and `mute`/`unmute` | `peer/voice.go`, `voice/src/main.rs`, `engine.rs` |
| 11 | Runtime voice commands: per-participant volume and mute, deafen, game volume, mic delay, device list and select | `peer/voice.go`; sidecar stdin commands |
| 12 | **Speaker identity:** tell the sidecar which SSRC belongs to which `clientId`, or rewrite SSRCs to a per-participant value | `peer/voice.go`, `voice/src/engine.rs` |
| 13 | Speaking levels and voice activity in the sidecar, batched into `voice.levels` | `voice/src/engine.rs`, `main.rs` |
| 14 | Device enumeration, live switching, and falling back to the default when a device disappears, without changing mode or mute | `voice/src/io.rs` |
| 15 | Server-side room tokens, moderator permissions and `room.setMaxStreams`, once the moderation model is decided. Until then `maxStreams` is a server setting | `signaling/server.go` |
| 16 | Diagnostics: per-link stats from Pion `GetStats`, plus `procstats`, plus a sidecar `stats` command for its jitter buffers | `peer`, `procstats`, `voice/src/main.rs` |
| 17 | **Voice on and off at runtime:** a client → server `voice {enabled}` message updating `Topology.Voice`; the core starts the sidecar on enable and stops it on disable. Today voice is fixed by `Join.Voice` | `proto`, `signaling/server.go`, `peer/voice.go` |
| 18 | **Room cap (D11):** `maxMembers`, default 10 (`cast signal -max-members`); `room_full` at join | `signaling/server.go`, `cmd/cast` |
| 19 | **TURN (D12):** `cast signal -turn` runs a TURN server (`pion/turn`) and hands out short-lived credentials in `welcome`; `hideIp` sets the relay-only ICE policy; `turn_unavailable` without a TURN server | `signaling`, `proto`, `peer/media.go` |
| 20 | **Degraded tiers (D14):** margin 1.1 before giving up, then unserved watchers wait with `noCapacity`; `unserved` and `maxBitrateKbps` in the topology | `signaling/election.go`, `signaling/server.go`, `proto` |

**Order.** Each step is a branch merged through a pull request; the reasons
are in [review-resolutions.md](../plans/review-resolutions.md#9-c5-c6-one-order).

1. **Multi-stream** ([plan](../plans/multi-stream.md)): #9 → #8 → #6–#7, with #18.
   In parallel, once its Phase 1 has merged: **#10** mic modes.
2. **#1 + #3** `cast agent` with pipe/file video input.
3. **#4–#5** identity, grace period, reconnect, failover rule.
4. **#19** TURN and hiding the IP.
5. **#17, #11–#13** voice at runtime.
6. **#14** devices, **#16** diagnostics.
7. **#20** with the tree election of [supernode-election.md](../plans/supernode-election.md).
8. **#15** moderation.

---

## Open questions

1. **Moderation model.** Is a streamer automatically a moderator, or is it
   whoever holds a moderator token? Does moderation survive that person
   stopping? Should a moderator be able to stop someone's mic at all
   (`room.serverMute`), given that otherwise only the user controls it? Who can
   change `maxStreams`? This decides the `room.*` methods and change #15.
2. **Watching while streaming (after v1).** D8 forbids it because a streamer's
   sidecar uses its own game audio only as the echo reference. Playing another
   stream's game audio as well needs a second game-audio input in the sidecar.
3. **Instant switching (after v1).** Switching streams rebuilds the link to the
   new tree. Keeping idle links warm (the existing live/standby track swap)
   would make switching back nearly instant, at the cost of more open
   connections per participant.
