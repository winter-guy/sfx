# Review of the protocol spec and plans: proposed resolutions

**Status:** decided, applied to the spec and plans in the same branch.
Resolves the review findings on
[cast/AGENT_PROTOCOL.md](../cast/AGENT_PROTOCOL.md),
[supernode-election.md](supernode-election.md), [mesh.md](mesh.md),
[multi-stream.md](multi-stream.md), [later.md](later.md) and
[CONSTITUTION.md](../CONSTITUTION.md). The spec and the plans are the source
of truth; this page keeps the reasoning. [Decisions needed](#decisions-needed)
lists the outcome of each question.

**Method.** For each finding: the options, what each costs, a choice, and a
second pass that tries to break the choice. Where a claim could be checked, it
was: the code paths cited below were read, and the tree search was prototyped
(numbers in [§ 1](#1-the-tree-search-misses-the-tree-it-advertises)).

---

## Contents

- [Summary](#summary)
- [Foundations: reconnect, identity, failover](#foundations-reconnect-identity-failover)
- [The agent protocol](#the-agent-protocol)
- [Supernode election](#supernode-election)
- [Mesh](#mesh)
- [Multi-stream plan and ordering](#multi-stream-plan-and-ordering)
- [Small fixes](#small-fixes)
- [Second pass: do the resolutions fit together?](#second-pass-do-the-resolutions-fit-together)
- [Decisions needed](#decisions-needed)
- [Edits, by file](#edits-by-file)

---

## Summary

| # | Finding | Resolution |
|---|---|---|
| 1 | Tree search can't find the tree it advertises | Search depth ≤ 3 always, by enumerating relay skeletons; prototyped |
| 2 | A signaling drop on a host triggers failover | Heartbeat silence alone no longer fails a host while its viewers say video flows |
| 3 | A 2-minute reconnect window holds slots for crashed peers | Server grace period of 15 s (`away` state); the client's 2-minute retry stays |
| 4 | `clientId` can be claimed by anyone | Server-issued resume token; a keypair later if moderation needs it |
| 5 | A stalled app blocks the core | One non-blocking writer, `seq` assigned at write, overflow → `core.resync`; backpressure goes to stdin |
| 6 | Voice can't be configured before it's enabled | Voice setters always allowed; `voice.setEnabled` takes the voice config; new server message |
| 7 | `video`/`gameAudio` watch flags have no server side | Flags in the `watch` message; audio-only watchers can't host and cost no video copy |
| 8 | Multi-stream Phase 3 breaks the build | Expand, migrate, contract: the server keeps `Role` as a shim until Phase 4 |
| 9 | Agent can't stream until change #3 | Ship #3 with #1; reorder the change list to match the multi-stream plan |
| 10 | 15 s keepalive trips the 3 s/6 s ICE timeouts | Drop the battery keepalive; keep 1 s; park it in later.md behind a measurement |
| C1 | mesh.md "decides" outside the spec | Room cap becomes proposed D11; the rest go back to "proposed" |
| C2 | "Hide my IP" needs TURN nobody runs | `cast signal -turn` (built-in `pion/turn`) and a per-member `hideIp` |
| C3 | Degraded mode: spec and plan disagree | Two tiers: drop the headroom first; then unserved watchers wait. The streamer is never pushed past its budget |
| C4 | Pure seeders break "one tree per member" | Pure seeding only for members who watch nothing |
| C5–C6 | Change order and mesh dependency disagree | Multi-stream doesn't wait for the mesh; one order everywhere |
| S1–S11, P1–P5 | Smaller spec and plan issues | [Small fixes](#small-fixes) |

---

## Foundations: reconnect, identity, failover

Findings 2, 3 and 4 are one problem: today a closed WebSocket **is** a leave
([`server.go`](../cast/internal/signaling/server.go) calls `removePeer` as soon
as the read loop ends), and a host with no heartbeat for 1.5 s **is** dead
(`hostSilentTimeout` in `checkHost`), even if its video is flowing. Both rules
assume signaling and media fail together. The spec says they don't ("a short
signaling outage doesn't interrupt media"). The fix is to let media evidence,
not the WebSocket, decide.

### 2. Heartbeat silence alone fails a healthy host

`checkHost` declares a host failed in three cases:

| Case | Rule today | Catches |
|---|---|---|
| a | No heartbeat for 1.5 s | A host whose signaling dropped, **even if its viewers are fine** |
| b | Most viewers stalled and no heartbeat for 0.5 s | A hung or crashed host (≈ 570 ms, measured) |
| c | Most viewers stalled for 1.5 s while the host receives | A host that receives but doesn't forward |

**Options:**

| Option | Effect | Verdict |
|---|---|---|
| A. Keep (a), raise it to the grace period | A signaling blip costs nothing for 15 s; a host whose viewers have no fresh status is caught late | Partial |
| B. Case (a) applies only when **no viewer of this host has fresh status**; otherwise trust the viewers | Hangs are still caught by (b) at ≈ 570 ms; a blip on a working host does nothing | **Chosen**, with A as the backstop |
| C. Send heartbeats over the media path | The server isn't on the media path | No |

**Resolution:** (a) becomes "no heartbeat for 1.5 s **and** no viewer of this
host reporting fresh status", plus a backstop at the grace period (15 s) for
the case where nobody reports. (b) and (c) don't change, so
`TestFailoverWhenSupernodeHangs` keeps its number.

**Second pass.** *Could a dead host now be kept?* A dead host stops sending,
so its viewers stall, and (b) fires at 0.5 s. The only case B delays is a
host whose viewers are all silent too. In that case nothing would help them
anyway, and the 15 s backstop covers it. *New test:* drop a host's WebSocket
while its media keeps flowing; assert no failover for 10 s.

### 3. Reconnect window

The spec mixes two numbers: how long the **client** keeps retrying (2 min) and
how long the **server** keeps a vanished peer's identity, stream slot, watching
and duties. They should differ.

**Options for the server grace period `G`:**

| `G` | A network blip | A crashed streamer | A crashed host |
|---|---|---|---|
| 0 (today) | Leave + rejoin: voice links torn down, stream slot lost, watchers churn | Slot freed at once | Backup takes over (≈ 20 ms) |
| 15 s | Invisible, if it heals within 15 s | Slot held 15 s; watchers see `away` | Stall rule (b) fails it over at ≈ 570 ms anyway |
| 2 min (spec today) | Invisible | **Slot blocked 2 min**, watchers stalled 2 min | Same as 15 s |

**Resolution: `G` = 15 s**, a server setting (`signal -grace 15s`).

- A closed or silent WebSocket marks the peer **`away`**. It keeps its stream
  slot, watching, duties and voice membership. Media links stay up.
- Others see `peer.updated` with `connection: "away"`, and `stream.updated`
  with `streamerAway: true` for its stream.
- Failover still runs on media evidence (§ 2), so a crashed host is replaced at
  ≈ 570 ms. Only the slot and identity wait.
- After `G` the peer is removed: `peer.left` with reason `timeout`, and its
  stream ends with `timeout`.
- The client keeps retrying for 2 min. Reconnecting within `G` resumes the
  peer. Reconnecting later is a fresh join with the same `clientId`: the core
  emits `stream.ended` for its own stream and `stream.state: idle`, but stays
  in the session from the app's point of view.

**Second pass.** *Can a crash be told apart from a blip, to free the slot
sooner?* A process crash usually closes the TCP connection at once, and a
network drop usually goes silent. But proxies and NATs blur this, so a guess
would sometimes free a live streamer's slot. 15 s of a held slot is cheap.
Don't guess. *Why 15 s?* It covers Wi-Fi roaming and router hiccups (seconds)
and stays under the 30 s ICE consent window, so a peer that is really gone has
lost its media too by then. Measure the real distribution of blip lengths once
reconnect exists.

### 4. Identity

`clientId` is generated by the app and stable per installation. That's right
for local settings (per-person volume), but the server must not let anyone
resume someone else's identity.

| Option | Stops | Cost | Verdict |
|---|---|---|---|
| A. Server issues a random **resume token** in `welcome`; resuming within `G` requires it | Hijacking a live or `away` identity | A field in `welcome` and `join` | **Now** |
| B. Per-installation keypair; `clientId` = hash of the public key; the server challenges on join | Also impersonation across sessions | Key storage in the app; a challenge round trip | **When moderation needs verified identity** (open question 1 of the spec) |
| C. Server-assigned IDs | Everything | Breaks stable per-person local settings | No |

**Rules with A:**

- Joining with a `clientId` that is live or `away`, without its token → error
  `client_id_in_use`. The app shows "you're already in this room on another
  device".
- With the token → the old connection is closed and the new one takes over.
- After `G` the identity is free. A wrong claim then only borrows someone's
  local volume settings on others' apps, which is harmless until moderation
  exists.

**Second pass.** *Two PCs with a copied config?* The second gets
`client_id_in_use` with a clear message; the app can offer "make a new
identity". *Token on stdio?* The core keeps it in memory only and never
passes it to the app.

---

## The agent protocol

### 5. A slow app must never stall the core

If the app stops reading stdout, the pipe fills and a blocking write freezes
whatever goroutine wrote. D2 promises the mic keeps working when the app
hangs, so the core must never block on the app.

**Design:**

1. **One writer goroutine** owns stdout. Everything else enqueues and never
   waits.
2. **Responses are never dropped.** They're bounded by the app's own requests:
   when more than 256 responses are queued, the core **stops reading stdin**.
   Backpressure lands on the app, which caused it, not on media.
3. **`voice.levels` is latest-only:** one slot, overwritten, written when the
   writer gets to it.
4. **State events** queue up to a bound (1,000 events). Past it, the writer
   drops the queued state events and writes one **`core.resync`** event. The
   app calls `session.get`.
5. **`seq` is assigned when an event is written**, not when it's produced. So
   merging levels or dropping events never leaves a silent gap, and a gap now
   only means a core bug. The app treats a gap like `core.resync`.
6. **stderr** gets the same treatment: a bounded buffer, dropped lines counted
   and reported in the next written line.

**Snapshot consistency.** A snapshot can include changes whose events are
written after it. That is safe if **every event is an idempotent state
replacement**: `peer.updated` carries the whole `Peer`, `stream.updated` the
whole `Stream`, `peer.left` deletes by `clientId`, and so on. The spec already
does this. Make it a stated rule: "applying an event twice, or after a
snapshot that already contains it, is harmless".

**Second pass.** *Is dropping state events worse than blocking?* Blocking
freezes the mic. Dropping costs one `session.get`. *Explicit event or gap
only?* Explicit: an app may ignore gaps, but it will see an event it doesn't
know and log it. Keep both.

### 6. Voice settings before voice is on

**Options:** (A) `voice.setEnabled` takes the full voice config; (B) the voice
setters work at any time and the config is stored until voice is enabled;
(C) both.

**Resolution: C.** The voice config always exists in the core, starting at the
defaults (`ptt`, not muted, default devices). Setters change it whether or not
voice is on. `voice.setEnabled({enabled, mode?, muted?, inputDevice?,
outputDevice?, micDelayMs?})` applies any given fields, then turns voice on or
off. The `voice_disabled` error goes away. `wrong_mode` stays: it catches a
real app bug.

**What turning voice on and off does:**

- **On:** start the sidecar, then send a new client → server message `voice
  {enabled}`. The server updates `Topology.Voice`, and voice links come up.
- **Off:** the server removes the peer from `Voice`, and the core **stops the
  sidecar**, which closes the mic device.

**Second pass.** *Why stop the sidecar instead of idling it?* An idle sidecar
keeps the mic device open, so macOS shows the mic-in-use indicator while the
user has voice off. That breaks trust in D2 more than a one-second restart
costs. *D9?* D9 protects voice from **streams**. Turning voice off is the user
ending it, which D9 allows.

**Change list:** add **#17: voice on/off at runtime** (`proto`, `server.go`,
`peer/voice.go`).

### S9. `error {fatal}` and `session.left` overlap

**Resolution:** `session.left` is the only signal that the session ended, and
it carries the cause: `{reason, error?: {type, message}}`. The `error` event
is for non-fatal problems only, and its `fatal` field goes away. If the core
itself can't go on, it exits, and the app sees stdout close.

### S6. The video source ends while live

| Source | On end or error |
|---|---|
| `pipe` | Pause the stream (`stream.updated` with `paused: true, pausedBy: "source"`); `error` with `source_failed`; reopen the pipe when a writer appears, then resume. The slot is kept, so an encoder restart in OBS costs nothing |
| `file` without `loop` | End the stream: `stream.ended` with reason `sourceEnded` |

Pausing changes stream state, not the mic, so it doesn't conflict with D2.
Watchers see `paused`, and pause never triggers failover
(`TestStreamerPauseDoesNotTriggerFailover`). The app can still `stream.stop`.

### S1. Versioning

**Resolution:** while the API is `0.x`, `core.hello` requires an **exact**
`major.minor` match. The "lower minor" rule starts at `1.0`. That removes the
contradiction with "any 0.x minor may break".

### S2. Null

**Resolution:** two rules, which already match how the spec uses null.

| Where | Absent | `null` |
|---|---|---|
| Requests (params) | Default, or unchanged for setters | Reset to the default (e.g. `micDelayMs: null`) |
| State (events, results, snapshots) | Never: every field is present | "None" (e.g. `watching: null`, `host: null`) |

### S3. Volume naming

Use **`volume`** in every name, with the value defined as a linear gain
(conventions table). Rename the `gain` params of `voice.setPeerVolume` and
`stream.setGameVolume` to `volume`. `gameVolume` and `Peer.local.volume`
already match.

### S4. Missing types

```jsonc
// VideoSink (watcher playback)
{ "kind": "pipe", "path": "/tmp/cast-out.h264" }   // Annex-B H.264
{ "kind": "none" }                                  // audio only, or testing
{ "kind": "native" }                                // future: in-app player

// Stats (diag.getStats): our own machine only
{
  "process": { "cpuPct": 4.1, "memMb": 52 },
  "sidecar": { "cpuPct": 2.0, "memMb": 18 },
  "links": [ { "clientId": "b77…", "kind": "video",           // video | voice
               "direction": "out", "path": "direct",          // direct | relay (TURN)
               "rttMs": 23, "lossPct": 0.1, "bitrateKbps": 8100 } ],
  "jitter": { "voiceMs": 40, "gameMs": 60 }
}
```

Every `Stats` field comes from something that exists today: `procstats`,
Pion's `GetStats`, and the sidecar's jitter buffers (the last one needs a
sidecar `stats` command, part of change #16).

### S5. Deafen

Add `voice.setDeafened({deafened})`, which mutes all voice playback locally,
and `VoiceState.deafened`. It does **not** mute the mic: D2 says the core
never changes the mic by itself. An app that wants Discord-style deafen calls
both, which is app policy.

### S7. `Peer.voice.speaking`

Remove it. Speaking changes several times a second, so it would flood
`peer.updated`. It lives only in `voice.levels`. `Peer.voice` keeps `inVoice`.

### S8. `Stream.reason` leaks bandwidth

Replace the free-text `reason` with `election: {reason}`, where `reason` is
one of `direct`, `keptHost`, `bestWatcher`, `streamerLastResort` or
`degraded`, plus later per-relay codes (later.md "Explain the tree"). It
carries no Mbps figures. A member's own numbers stay in `diag.getStats`.

### S10. Change #2 is unnecessary

Go lets any package under `cast/` import `cast/internal/...`. Put the agent at
`cast/internal/agent` and drop change #2. Revisit only if an outside Go
program needs the core as a library.

### S11. Per-person volume at join

Let `voice.setPeerVolume` and `voice.setPeerMuted` accept any `clientId`, in
the room or not: the core remembers the value for the session. Add an
optional `JoinParams.peers: { "<clientId>": {volume, muted} }` to set them all
at once. `unknown_peer` then applies only to `room.*`. No more full-volume
moment between `peer.joined` and the app's setter.

---

## Supernode election

### 1. The tree search misses the tree it advertises

With the default streamer budget of one copy, a depth-2 tree is S → one relay
→ everyone. That's the star. The worked example's best tree (Option 2) has
depth 3. The algorithm only tries depth 3 when depth 2 is infeasible, so it
returns the star.

**Options:**

| Option | Exact? | Cost | Verdict |
|---|---|---|---|
| A. Exact search over depth ≤ 3, always | Yes | Enumerate relay skeletons; see below | **Chosen** |
| B. Compact Tree greedy + local swaps, always | No | Trivial | Fallback only |
| C. Integer programming solver | Yes | A dependency for a 10-node problem | No |

**The search.** For a fixed **skeleton** (which watchers sit at depth 1 under
the streamer, and which relays sit at depth 2 under them), every node's latency
is fixed. Every remaining watcher then just needs a parent with a free copy
within the threshold `T`. That's a bipartite assignment with capacities,
solved exactly by max-flow. So: enumerate skeletons over **relays only**
(watchers with copies > 0), binary search `T` per skeleton, and keep the best.
Branch and bound: skip a skeleton whose own relays are already no better than
the best tree found.

**Prototype** (Python, on the worked example and two synthetic rooms):

| Case | Result | Skeletons | Time (Python) |
|---|---|---|---|
| Example, budget 1, depth ≤ 2 (today's plan) | **42 ms** (the star) | 9 | 0.1 ms |
| Example, budget 1, depth ≤ 3 | **34 ms** (Option 2's worst latency) | 37 | 0.3 ms |
| Example, budget 2, depth ≤ 3 | **27 ms** (Option 3) | 261 | 2 ms |
| Mixed seeders, budget 1 | — | 1,025 | 4 ms |
| Worst case: all 9 watchers relay, budget 2 | — | 81,036 | 274 ms |

Go should be one to two orders of magnitude faster than this Python, so even
the worst case looks like a few milliseconds. That is an estimate: measure it.
Guard it anyway: if more than 6 watchers are relays, keep the 6 with the
lowest latency to the streamer (ties: most copies), or stop at a 5 ms budget
and use the greedy result.

**The prototype also confirms** the counting fix: budget 2 at depth 2 is
C(9,1) + C(9,2) = **45** skeletons, not 54.

**Spec edits in supernode-election.md:** Summary point 1 "depth 2, at most 3"
→ "depth ≤ 3, chosen by the same exact search"; `best_depth2` →
`best_tree(max_depth=3)`; the one-line feasibility bound gets a depth-3 term;
"54" → "45". Ties in the prototype still need the churn and mean tie-breakers;
it only minimises the worst latency.

### C3. Degraded mode

The spec serves everyone on a best-effort basis. The election plan says the
streamer must never silently fall back to sending to everyone. Today's
degraded pick can make the streamer host.

**Options:**

| Option | Who suffers | Verdict |
|---|---|---|
| A. Overload a relay anyway (today) | Everyone under that relay: loss, then keyframe requests, then worse for all | No |
| B. Push the streamer past its budget | The streamer's game (bufferbloat) | No (D7) |
| C. Unserved watchers wait; the streamer sees `r*` | The extra watchers, visibly | Yes, as the last tier |
| D. First lower the 1.3 headroom to 1.1 | Small risk on keyframe bursts | Yes, as the first tier |

**Resolution:** D, then C.

- **Tier 1 (`degraded: "tight"`):** re-run the election with `h = 1.1`. The
  headroom is a safety margin, not a requirement, so most rooms that miss
  by one copy fit here. Not 1.0: upload is measured as payload (the speed
  test runs over TCP), while RTP, SRTP, UDP and IP headers add about 3–5% on
  the wire, so `h = 1.0` would overcommit the uplink. The remaining ≈ 5%
  absorbs part of a keyframe burst.
- **Tier 2 (`degraded: "full"`):** serve as many watchers as fit. The rest
  get `stream.state: waiting` with `reason: "noCapacity"`. `Stream` carries
  `unserved` and `maxBitrateKbps` (`r*`), and the app offers the streamer a
  lower bitrate or asks for seeders.
- The streamer's budget is never exceeded automatically.

**Second pass.** *Does tier 1 break seeding consent?* No: a seeding cap is in
Mbps, and with `h = 1.1` the real traffic `r × copies` still stays under it.
*Who waits?* The latest to start watching, so nobody already playing loses
their picture.

### C4. Pure seeders break "one tree per member"

mesh.md proves media flows one way per pair, and design B needs only one
outgoing video track per pair. Both rest on every member being in at most one
tree. `on` + `anyStream` ("also relay a stream I'm not watching") puts a
watcher of X into Y's tree too.

| Option | Verdict |
|---|---|
| A. Pure seeding only for members who **watch nothing** (`seedOnly`): an idle member, or a cloud seeder | **Chosen**: keeps the invariant, keeps "bring your own seeder" |
| B. Allow it, with two video tracks per pair and the proof rewritten | Doubles the seeder's download (8.3 Mbps more) for a rare gain in a room of 10 |
| C. Drop pure seeders | Loses the cloud seeder |

Restate the invariant everywhere: **each member is in at most one tree: as
streamer, as watcher, or as seed-only relay.** later.md's "watching while
streaming" breaks it too. Note that there, since it would need a second track
per pair.

### P1. Wi-Fi factor 0.6

It's unmeasured, and it double-counts: the upload probe and "proven" rate
already measure what Wi-Fi actually delivers. **Resolution:** drop `w_i` from
the capacity formula. Keep Wi-Fi as a reliability penalty `p_i` (its problem is
variance, not the average), and label `p_i`'s values as assumptions until
measured.

### P2. The pre-promotion probe

Padding over "idle links" needs mesh design B, and padding on a link that
carries live video can cause the loss it's measuring. **Resolution: slow
start.** Promote one copy at a time. A node at `k` proven copies gets one more
child, the server watches that child's receiver reports and the node's RTT for
5 s, and rolls back on loss or an RTT rise. It uses real traffic and works
without the mesh. The worst case is a short glitch for one child. The cost is
slower convergence (a node goes from 1 to 4 copies in about 15 s). That's
acceptable because placement starts from the join probe, so a node only
slow-starts beyond what it has measured.

### P3. Seeding default

Today `canHost` is already opt-in (`-host` defaults to false), so "off" is not
a regression. The weak point is when to ask. **Resolution:** default `off`,
asked **when it matters**: when a stream the member watches is degraded, the
app prompts "this stream needs 2 more helpers. Share up to 1 viewer's worth
(≈ 3.7 GB per hour)?". It asks at that moment, with the cost shown, rather than
once at install.

---

## Mesh

### 10. Battery keepalive

`cast` builds every connection with `SetICETimeouts(3s, 6s, 1s)`
([`media.go`](../cast/internal/peer/media.go)). With a 15 s keepalive, an idle
pair hears nothing for 15 s and goes failed after 6 s. That is exactly the
argument mesh.md uses against turning the keepalive off.

**Options:**

| Option | Cost | Gain | Verdict |
|---|---|---|---|
| A. 15 s keepalive with timeouts raised to ≈ 35 s / 45 s on those pairs | Dead idle pairs noticed after ≈ 45 s; per-pair settings; rebuild pairs on power change | Only when **both** ends are on battery: a 1 s peer still wakes the battery side every second | No |
| B. Keep 1 s everywhere | Nothing new | — | **Chosen** |
| C. Measure first: energy impact of an idle 10-member mesh on a laptop (`powermetrics` on macOS) | A test | Knowing whether it matters | Then decide |

The measured gain is 0.17% of a core per member. It only appears when both
ends are on battery, and it costs three new mechanisms. **Resolution:** keep
1 s, move the idea to later.md with "needs a battery measurement showing real
drain", and mark it in mesh.md as not adopted.

### C1. "Decided" in mesh.md

| Item | Resolution |
|---|---|
| Room cap 10 | Independent of the mesh, and already the basis of every capacity table. Propose **D11** in the spec: `maxMembers`, a server setting, default 10; `room_full` error |
| Hide my IP | Proposed, depends on C2 |
| Battery keepalive | Not adopted (§ 10) |
| Mesh design B itself | Proposed until measured on real hardware (mesh.md's own recommendation 6) |

### C2. "Hide my IP" needs a TURN server

**Options:** the project hosts TURN; each group hosts it; the signaling server
ships it.

**Resolution:** `cast signal -turn` runs a TURN server in the same binary
(`pion/turn`, already in `go.sum` as an indirect dependency). The signaling
server hands out short-lived TURN credentials in `welcome`. Whoever runs the
signaling server already runs a VPS, and now gets TURN with it, the same model
as "bring your own seeder". `JoinParams.network.ice` can still override.

In the core: `network.hideIp: true` → relay-only ICE policy, applied at join
(mesh.md explains why it can't change later). Without a TURN server,
`session.join` fails with **`turn_unavailable`** instead of silently exposing
the IP.

**Second pass.** *Bandwidth bill?* It lands on whoever runs the signaling
server: ≈ 4 GB per hidden streamer-hour (mesh.md). Show it in the server log.
*UDP 3478 and a relay port range must be open* on the VPS. Document it with
the flag.

### P4. mDNS for LAN addresses

Rejected on a second look. `media.go` disables mDNS on purpose, and LAN play is
a main use case. mDNS fails on networks that block multicast, which would push
same-LAN friends onto hairpin NAT paths that many routers don't support. A
private LAN address reveals little. Keep host candidates.

---

## Multi-stream plan and ordering

### 7. Watch flags

**Resolution:** carry them in the protocol now, so `proto` doesn't change
twice.

- `watch` message: `{streamer, video, gameAudio}`. Sending `watch` again for
  the same streamer only changes the flags (the spec's idempotence rule).
  Both false → invalid params; that's `unwatch`.
- Server: only `video: true` watchers are host candidates. An audio-only
  watcher is still a child in the tree. It costs its parent 128 kbps, not a
  video copy, so it's counted in `B_i` but not in copies.
- Client: the parent's video sender for an audio-only child stays on the idle
  track, the same swap the backup uses.
- The single-watcher "direct" rule counts **video** watchers.

Server parts go in Phase 3, client parts in Phase 4. They add little to
either.

### 8. Phase 3 breaks the build

**Resolution: expand, migrate, contract.**

1. **Phase 3 (expand):** add the new messages and `Topology.Streams` while
   **keeping** `Join.Role` and the single-stream fields. The server maps
   `Role: streamer` to an automatic `stream start` and `Role: viewer` to an
   automatic `watch` of the only stream (or of the first that starts), and
   fills the old fields from that stream. Every existing integration test
   then runs **unchanged** against the new server, which is the best
   regression check this phase can have.
2. **Phase 4 (migrate):** the client and CLI move to the new messages.
3. **Contract:** one commit removes `Role` and the old fields.

### 9, C5, C6. One order

- **The mesh doesn't block multi-stream.** Mesh design B needs real-hardware
  measurements first. Phase 4 reuses today's link code (`reconcileLocked`
  unchanged), so the cost of switching to the mesh later is contained.
  supernode-election.md's "decide the mesh before Phase 4" becomes "the mesh
  replaces Phase 4's link building later, behind the same `reconcileLocked`
  entry point".
- **#3 ships with #1:** without a non-stdin video source, an agent can't go
  live.

**The order (decided).** Each step is a branch merged through a pull request
(Constitution, Working 2).

| Step | Branch | Changes | Why here |
|---|---|---|---|
| 1 | `feat/multi-stream` | Phases 1–5: #9 sidecar game mode → #8 election order → #6–#7 server and client, with the watch flags (§ 7), the room cap **D11** (`maxMembers`, `room_full`) next to `maxStreams`, and the expand/contract shim (§ 8) | Changes the model that every later step exposes. Doing the API or reconnect first would mean building them twice |
| 1b | `feat/mic-modes` (in parallel with step 1) | #10 `ptt`/`open` in the sidecar | Sidecar-only and small. Start it after Phase 1 merges, since both add sidecar stdin commands |
| 2 | `feat/agent` | #1 `cast agent` + #3 pipe/file video input + stdout backpressure (§ 5) + the spec fixes S1–S11 | First usable API: an app can be built against it from here on. Reconnect fields and events arrive later as additive changes (Principle 6) |
| 3 | `feat/reconnect` | #4–#5: resume token, 15 s grace period and `away`, the failover rule (§ 2–4) | Server peer lifecycle; easier on the per-stream server from step 1. `connection.state` events plug into the agent from step 2 |
| 4 | `feat/turn` | `cast signal -turn`, TURN credentials in `welcome`, `hideIp`, `turn_unavailable` (C2) | Touches `welcome`/`join` right after step 3 changed them, so it doesn't conflict with it |
| 5 | `feat/voice-runtime` | #17 voice on/off, #11 runtime voice commands, #12 speaker identity, #13 levels, S5 deafen | Completes the voice part of the API |
| 6 | `feat/devices`, `feat/diag` | #14 devices; #16 diagnostics | #16's per-link stats are the metrics the tree election needs next |
| 7 | `feat/tree-election` | supernode-election.md: depth ≤ 3 search (§ 1), seeding modes and the just-in-time prompt (P3), slow start (P2), degraded policy (C3) | Needs #16's metrics, and the mesh decision for its failover design |
| 8 | `feat/moderation` | #15 | Waits on the spec's open question 1 |

**Alongside, not blocking:** measure the mesh on a Windows gaming PC and over
real networks (mesh.md recommendation 6) during steps 1–3, so the mesh
decision is ready by step 7.

---

## Small fixes

| # | File | Fix |
|---|---|---|
| A1 | supernode-election.md:129 | 54 → 45 |
| A3 | supernode-election.md:43, 195, 361, 631 | ICE keepalive: Pion's default is 2 s, `cast` sets **1 s** |
| A4 | mesh.md:187, 249; supernode-election.md:592 | 0.65% → 0.7% (0.72% measured) |
| A6 | AGENT_PROTOCOL.md:256 | ≈ 63 → ≈ 64 Mbps |
| A7 | (history) | The two commits lack the `docs:` scope and bundle several documents. They're pushed, so leave them; follow the rule from now on |
| P5 | multi-stream.md Phase 4 | `cast watch -stream X`: X matches a peer ID or a name; an ambiguous name exits with the list of matches and their IDs |

---

## Second pass: do the resolutions fit together?

| Interaction | Check |
|---|---|
| Grace period (§ 3) × failover rule (§ 2) | An `away` host with flowing video stays host; a dead one is replaced by stall rule (b) at ≈ 570 ms. An `away` streamer: its host sees no video, so the "streamer is the problem" rule stops a pointless host switch (existing logic) |
| Resume token (§ 4) × grace period | The token is valid only while the identity is live or `away`; after `G`, the identity is free, by design |
| `seq` at write (§ 5) × idempotent events × null rules (S2) | State events always carry whole objects with every field present, so replaying one is harmless |
| Degraded tier 1 (C3) × seeding cap | Caps are in Mbps of real traffic, so `h = 1.1` never exceeds a member's consent |
| Seed-only (C4) × watch flags (§ 7) | A seed-only relay has no `watching`; it's in one tree as a relay. Audio-only watchers are leaves. The invariant holds |
| Voice off stops the sidecar (§ 6) × D9 | D9 covers streams, not the user's own voice switch |
| Slow start (P2) × degraded (C3) | A degraded stream may need promotions; slow start makes recovery take seconds, not a moment. Acceptable: tier 1 keeps people playing meanwhile |
| Expand/contract (§ 8) × "green before commit" | Every commit builds and passes; the shim is deleted in its own commit |
| Built-in TURN (C2) × D1 (stdio only) | D1 is about the app ↔ core API; TURN is a media relay. No conflict |

**Residual risks:**

- The 15 s grace period and the 6-relay search cap are judgement calls.
  Measure both once implemented.
- Slow start trusts receiver reports. A lying child could block a relay's
  promotion. That limits capacity but can't hurt anyone else's video.
- Built-in TURN makes the signaling host pay the bandwidth bill. That's
  stated, not solved.

---

## Decisions needed

| # | Question | Outcome |
|---|---|---|
| 1 | Server grace period for a vanished peer | **Decided:** 15 s, configurable |
| 2 | Room cap as **D11** (`maxMembers` 10, `room_full`) | **Decided:** yes |
| 3 | Pure seeders only when watching nothing | **Decided:** yes |
| 4 | Degraded: lower the headroom to 1.1 first, then make extra watchers wait; never exceed the streamer's budget | **Decided:** option A with `h = 1.1` |
| 5 | Battery keepalive | **Decided:** dropped; parked in later.md |
| 6 | TURN for "hide my IP" | **Decided:** built into `cast signal -turn` |
| 7 | Seeding prompt | **Decided:** default off, asked when a watched stream is short of relays |
| 8 | Change order | **Decided:** [the table above](#9-c5-c6-one-order) |

---

## Edits, by file

| File | Edits |
|---|---|
| `cast/AGENT_PROTOCOL.md` | Versioning (S1); null rules (S2); `volume` naming (S3); `VideoSink`, `Stats` (S4); `voice.setDeafened` (S5); drop `Peer.voice.speaking` (S7); `Stream.election` (S8); `session.left.error`, no `fatal` (S9); per-person settings for any `clientId`, `JoinParams.peers` (S11); `voice.setEnabled` with config, no `voice_disabled` (§ 6); stdout rules, `core.resync`, idempotent events (§ 5); `away`, `streamerAway`, grace period, resume rules, `client_id_in_use` (§ 3–4); source end (S6); degraded tiers, `unserved`, `maxBitrateKbps` (C3); `hideIp`, `turn_unavailable` (C2); D11 and `room_full` (C1); change list: drop #2, add #17, new order; capacity ≈ 64 |
| `plans/supernode-election.md` | Depth ≤ 3 search and the prototype numbers (§ 1); 45; 1 s keepalive; drop `w_i` (P1); slow start replaces the padding probe (P2); seeding prompt (P3); seed-only (C4); degraded tiers (C3); mesh doesn't block Phase 4 (C6) |
| `plans/mesh.md` | One-tree invariant restated (C4); "Decided" → proposed, except the room cap → D11 (C1); battery keepalive not adopted (§ 10); 0.7% |
| `plans/multi-stream.md` | Expand/contract phases (§ 8); watch flags (§ 7); `-stream` matching (P5); link to the mesh as a later replacement |
| `plans/later.md` | Battery keepalive (with its precondition); note that watching while streaming breaks the one-tree invariant |
| `cast/internal/signaling/server.go` (with change #4–#5) | Failover rule (§ 2), `away` and grace period (§ 3), resume token (§ 4) |
