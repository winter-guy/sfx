# cast agent protocol: JSON-RPC 2.0 over stdio

**Status:** draft v0.4, not implemented yet. This spec defines the control
protocol of `cast agent`: how an **app** (a desktop UI, a bot, a test harness)
drives the **core** (`cast` plus its `voice` sidecar) as a child process,
sending JSON-RPC requests on stdin and receiving responses and events on
stdout. [Changes required](#changes-required-in-the-current-code)
lists what the code needs before the API can exist.

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
`seq` that increases by 1, so a gap means events were missed:

```json
{"jsonrpc":"2.0","method":"peer.joined","params":{"seq":42,"peer":{ "clientId":"…", "name":"ana", "…":"…" }}}
```

Conventions:

| Kind | Convention |
|---|---|
| Names | Methods and events are `area.camelCase`; fields are `camelCase` |
| Time | Durations in **milliseconds** (`…Ms`); timestamps as RFC 3339 strings in UTC |
| Bitrates | **kbps** (`…Kbps`) |
| Gain | Linear multiplier: `1.0` = unchanged, `0` = silent, maximum `2.0` |
| Levels | dBFS, from `-127` (silence) to `0` (full scale) |
| Missing optional fields | Mean "default" or "unchanged", never null |

---

## Handshake and versioning

1. The core starts and immediately emits **`core.ready`**:

   ```json
   {"jsonrpc":"2.0","method":"core.ready","params":{"seq":1,"apiVersion":"0.4","coreVersion":"0.5.0",
     "capabilities":["stream","watch","gameAudio","voice"]}}
   ```

   `capabilities` lists what this build supports. For example, `voice` is
   missing if the sidecar isn't installed.

2. The app calls **`core.hello`** before anything else:

   ```json
   → {"jsonrpc":"2.0","id":1,"method":"core.hello","params":{"apiVersion":"0.4","client":"cast-desktop/1.2.0"}}
   ← {"jsonrpc":"2.0","id":1,"result":{"apiVersion":"0.4"}}
   ```

- **Versions** are `major.minor`. Same major: compatible; the core answers with
  the lower of the two minors and doesn't use newer features. Different major:
  `core.hello` fails with `version_mismatch`.
- While the API is `0.x`, any minor version may break compatibility.

---

## Concepts and identifiers

| Concept | Description |
|---|---|
| **Session** | Membership in one room on one signaling server, from `session.join` to `session.left`. It survives reconnects. One per core process (D10). |
| **`clientId`** | A stable identifier the **app** generates (UUID), stored per installation and sent on join. Peers are identified by `clientId` everywhere in this API. |
| `peerId` | The server's connection ID (`p12`). It changes on every reconnect, so it's for diagnostics only. |
| **Participant** | Everyone in the room. Anyone can talk, watch one stream, and start a stream while the room is below `maxStreams`. |
| **Stream** | One participant's live video plus optional game audio. Identified by the **streamer's `clientId`**, since a participant has at most one stream. Each stream has its own supernode tree. |
| **Watching** | Per participant: **at most one** stream, chosen explicitly, plus whether to receive its **video** and its **game audio**. Someone can be in the room for voice only. |
| **Duties** | Assigned by the server per stream and changing over time: `host` (supernode) and `backup`. Held by that stream's video watchers who opted in with `canHost` and have enough upload; the streamer only as a last resort (D7). |
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
5. Otherwise the stream is **degraded**: everyone is still served on a best
   effort basis, and the app can show it.

A watcher who only takes game audio (`video: false`) can't host, because it
doesn't receive the video.

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

**A room of 10, streamers not watching (D8):**

| Streams | Watchers | Split | Biggest host needs | Each streamer | Room total upload |
|---|---|---|---|---|---|
| 1 | 9 | 9 | 85 Mbps | 10.6 | ≈ 95 Mbps |
| 2 | 8 | 4 + 4 | 32 Mbps | 10.6 | ≈ 85 Mbps |
| 3 | 7 | 3 + 2 + 2 | 21 Mbps | 10.6 | ≈ 74 Mbps |
| 4 | 6 | 2 + 2 + 1 + 1 | 10.6 Mbps | 10.6 | ≈ 63 Mbps |
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
stream because hosting looks impossible: the degraded state reports that, and
it changes as people switch streams.

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
| `session.join` | `JoinParams` | `{ self: Peer, room: Room }` | Resolves once the server has welcomed us. Fails with `already_in_session` if already in one |
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
    "canHost": true,                  // volunteer as supernode for the stream we watch
    "ice": [{"urls":["stun:stun.l.google.com:19302"]},
            {"urls":["turn:turn.example.com:3478"],"username":"u","credential":"p"}]
  },
  "playback": {
    "video": {"kind":"pipe","path":"/tmp/cast-out.h264"}, // until the in-app player exists
    "gameVolume": 1.0
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
| `stream.setWatching` | `video?`, `gameAudio?` | `{}` | Change what we receive from the stream we watch. With `video: false` the host stops sending us video, which saves its upload, and we can no longer host |
| `stream.setGameVolume` | `gain` | `{}` | Local playback volume of game audio; only affects this device |

### Voice

| Method | Params | Result | Notes |
|---|---|---|---|
| `voice.setEnabled` | `enabled` | `{}` | Join or leave the voice mesh without leaving the room. The only way, besides leaving the room, to end voice (D9) |
| `voice.setMode` | `mode`: `"ptt"` \| `"open"` | `{}` | Takes effect immediately for this session. **The app saves it** as the user's default for next time. Switching to `ptt` stops transmitting until the key is pressed; switching to `open` uses the current `muted` value |
| `voice.ptt` | `pressed` | `{}` | `ptt` mode: key down / key up. Applied within 20 ms |
| `voice.setMuted` | `muted` | `{}` | `open` mode: mute or unmute. Applied within 20 ms |
| `voice.setPeerVolume` | `clientId`, `gain` | `{}` | Local only; others are unaffected |
| `voice.setPeerMuted` | `clientId`, `muted` | `{}` | Local only |
| `voice.setMicDelay` | `ms` (or `null` for the default) | `{}` | Echo-cancellation tuning while streaming |

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
| `connection.state` | `state`, `attempt?`, `retryInMs?`, `reason?` | Signaling connection changes. `state`: `connecting`, `connected`, `reconnecting`, `disconnected` |
| `session.joined` | `self: Peer`, `room: Room` | After join (also returned by `session.join`) |
| `session.left` | `reason` | `left` (we asked), `kicked`, `serverClosed`, `failed` (gave up reconnecting) |
| `room.updated` | `room: Room` | A room setting such as `maxStreams` changed |
| `peer.joined` | `peer: Peer` | Someone joined the room |
| `peer.left` | `clientId`, `reason` | Someone left (`left`, `timeout`, `kicked`) |
| `peer.updated` | `peer: Peer` | A participant's duties, streaming, watching or voice state changed |
| `stream.started` | `stream: Stream` | Someone went live |
| `stream.updated` | `stream: Stream` | A stream paused or resumed, turned game audio on or off, changed bitrate, gained or lost watchers, changed host or backup, or became degraded |
| `stream.ended` | `streamer`, `reason` | A stream ended: `stopped`, `left`, `timeout` |
| `stream.state` | `state`, `streamer?`, `fromClientId?` | Our own playback: `idle` (not watching), `waiting`, `playing`, `paused`, `stalled`, `switching` (failover in progress). `fromClientId` is who we receive from |
| `voice.state` | `VoiceState` | Our own voice state changed (always as a result of an app call, a moderator action, or a device change) |
| `voice.levels` | `levels: [{ clientId, levelDb, speaking }]` | **Up to 10 per second** while anyone is speaking. Includes ourselves. Batched to avoid flooding |
| `voice.link` | `clientId`, `state` | A voice connection to a participant: `connecting`, `connected`, `failed` |
| `audio.devicesChanged` | `inputs`, `outputs` | Devices were added or removed. If the selected device vanished, the core falls back to the default and reports it in `voice.state` |
| `error` | `type`, `message`, `fatal` | A problem not tied to a request. `fatal: true` means the session ended |

---

## Data types

```jsonc
// Room
{ "name": "friday", "maxStreams": 4 }

// Peer
{
  "clientId": "6f1c…",
  "peerId": "p12",                   // diagnostics only; changes on reconnect
  "name": "ana",
  "self": false,
  "streaming": false,                // has a live stream
  "watching": { "streamer": "a91…", "video": true, "gameAudio": true },   // null when not watching
  "duties": ["host"],                // [] | ["host"] | ["backup"], for the stream it watches
  "voice": { "inVoice": true, "speaking": false },
  "local": { "volume": 1.0, "muted": false }   // this app's local settings for that participant
}

// Stream (one per live participant)
{
  "streamer": "a91…",                // clientId; also the stream's identifier
  "startedAt": "2026-11-20T19:02:11Z",
  "paused": false,
  "gameAudio": true,
  "bitrateKbps": 8000,
  "fps": 60,
  "watchers": ["6f1c…", "b77…"],     // clientIds watching (video or game audio)
  "host": "clientId|null",           // the streamer itself when it sends directly
  "backup": "clientId|null",
  "degraded": false,                 // nobody, the streamer included, can serve every watcher
  "reason": "supernode ana (450/31 Mbps)"
}

// VoiceState (our own)
{
  "enabled": true,
  "mode": "ptt",                     // "ptt" | "open"
  "pttPressed": false,               // ptt mode
  "muted": false,                    // open mode
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

// VideoSource (streamer)
{ "kind": "pipe",   "path": "/tmp/cast-video" }      // Annex-B H.264 from a named pipe or file
{ "kind": "file",   "path": "clip.h264", "loop": true }
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
| `voice_disabled` | A voice call while voice isn't enabled |
| `unauthorized` | Bad or missing room token |
| `forbidden` | Not allowed (e.g. kick without moderator rights) |
| `unknown_peer` | `clientId` not in the room |
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

**Reconnect.** If the signaling connection drops, the core emits
`connection.state: reconnecting` and retries with exponential backoff from
0.5 s to 10 s, with jitter, for up to 2 minutes. Then it emits
`session.left: failed`. On reconnect it rejoins with the same `clientId`, and
others see `peer.updated`, not leave + join. A streamer keeps its stream slot,
and a watcher keeps watching the same stream, if it reconnects within the
window. Media links that survived keep running; failed ones are rebuilt. Mic
mode and mute are unchanged.

**Media keeps going.** Video and voice flow peer to peer, so a short signaling
outage doesn't interrupt them. Only topology changes wait for the reconnect.

**Ordering.** Events are emitted in `seq` order. A response may arrive before
or after the events its request caused. Use the events as the source of truth
for state.

**Idempotent setters.** Calling a setter twice with the same value is fine and
emits at most one event. `stream.watch` for the stream already watched changes
only the `video` and `gameAudio` flags.

**Latency.** `voice.ptt`, `voice.setMuted` and `voice.setEnabled(false)` take
effect within 20 ms of the request being read. `voice.levels` is at most 100 ms
old.

**Local settings are local.** Mic mode and mute preference, per-participant
volume and mute, and game volume aren't sent to anyone, and the core doesn't
persist them across restarts; the app does.

---

## Example session

`→` app to core, `←` core to app (one JSON message per line on the wire; shown
abbreviated). The user switched to open mic in an earlier run, so the app
passes `"mode":"open"`. Two participants, `a91…` and `b77…`, stream during the
session.

```
← {"method":"core.ready","params":{"seq":1,"apiVersion":"0.4","capabilities":["stream","watch","gameAudio","voice"]}}
→ {"id":1,"method":"core.hello","params":{"apiVersion":"0.4","client":"cast-desktop/1.2.0"}}
← {"id":1,"result":{"apiVersion":"0.4"}}
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
← {"method":"stream.updated","params":{"seq":7,"stream":{"streamer":"a91…","watchers":["6f1c…"],"host":"a91…","reason":"single watcher: direct","…":"…"}}}
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
| Reconnect | All of it | Show "reconnecting…" |
| TURN | Use configured servers | Supply servers and credentials |
| **Mic mode and mute** | Apply `voice.setMode`, `voice.ptt`, `voice.setMuted` within 20 ms; never change them by itself | Start in `ptt` on first install; **save the user's mode (and open-mic mute) and pass it on every join**; global hotkey, buttons |
| Streaming video | `stream.*`: go live, pause, game audio on/off; the room's stream limit is enforced by the server | Go-live UI, source picker; show who's streaming when the limit is hit |
| Watching | One stream at a time, switching trees, per-stream election | Stream list and picker, video/game-audio toggles |
| Who is speaking | Map voice streams to `clientId`; compute levels; `voice.levels` | Draw indicators |
| Per-person volume / mute | Per-participant gain in the mixer | Slider; remember the setting locally |
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
| 1 | Add a `cast agent` command: JSON-RPC over stdio, an event bus with `seq`, state snapshots | `cast/cmd/cast`, new `cast/agent` package |
| 2 | Move what the agent needs out of `internal/` (or keep the agent inside the module) | `cast/internal/peer` |
| 3 | Video input other than stdin: named pipe / file in agent mode | `peer.FrameSource` |
| 4 | **Stable `clientId`** in `join`; the server lets a reconnecting client resume its identity, a streamer keep its stream, and a watcher keep watching | `proto.Join`, `signaling/server.go` |
| 5 | **Signaling reconnect** with backoff, keeping media links alive | `peer/client.go` (today `Run` returns and the process exits) |
| 6 | **Several streams per room:** one stream per streaming participant, each with its own election, host, backup, epoch, suspects and failure monitor. `stream.start` / `stream.stop` claim and release a slot up to `maxStreams` (server-enforced) instead of a role fixed at join. Topology becomes a list of streams. Pause, and game audio on/off. Today a room has exactly one streamer | `proto`, `signaling/server.go`, `peer/client.go` |
| 7 | **Watching:** explicit and one stream at a time. The server puts a watcher in that stream's tree only; the host sends only to its watchers; the election counts only that stream's video watchers. Switching leaves the old tree, with a planned failover if the participant was its host. Today joining a room means watching | `proto`, `signaling/server.go`, `signaling/election.go`, `peer/links.go` |
| 8 | **Election order:** watchers first, the streamer only as a last resort (D7). The single-watcher rule stays. Today the streamer is preferred right after the current host | `signaling/election.go` |
| 9 | **Live game mode in the sidecar:** a `game play\|reference\|off` stdin command (with the mic delay following the mode), so starting, stopping or switching streams never restarts the sidecar and never drops voice (D9). Today the mode is fixed by `--game` at startup | `peer/voice.go`, `voice/src/main.rs`, `engine.rs` |
| 10 | **Mic modes:** `ptt` and `open` in the sidecar (transmit gate driven only by commands from `cast`), applied within 20 ms; mode and mute passed in at start. Today the sidecar has only `--muted` and `mute`/`unmute` | `peer/voice.go`, `voice/src/main.rs`, `engine.rs` |
| 11 | Runtime voice commands: per-participant gain and mute, game volume, mic delay, device list and select | `peer/voice.go`; sidecar stdin commands |
| 12 | **Speaker identity:** tell the sidecar which SSRC belongs to which `clientId`, or rewrite SSRCs to a per-participant value | `peer/voice.go`, `voice/src/engine.rs` |
| 13 | Speaking levels and voice activity in the sidecar, batched into `voice.levels` | `voice/src/engine.rs`, `main.rs` |
| 14 | Device enumeration, live switching, and falling back to the default when a device disappears, without changing mode or mute | `voice/src/io.rs` |
| 15 | Server-side room tokens, moderator permissions and `room.setMaxStreams`, once the moderation model is decided. Until then `maxStreams` is a server setting | `signaling/server.go` |
| 16 | Diagnostics: per-link stats from Pion `GetStats`, plus `procstats` | `peer`, `procstats` |

Suggested order: 1 → 4–5 → 9–10 → 6–8 → 11–13 → 3 → 14 → 16 → 15.

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
