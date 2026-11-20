# Plan: several streams per room

**Implements:** decisions D5–D9 and D11 of [cast/AGENT_PROTOCOL.md](../cast/AGENT_PROTOCOL.md#decisions),
changes #6–#9 and #18 of its [change list](../cast/AGENT_PROTOCOL.md#changes-required-in-the-current-code).
Step 1 of the spec's order. Mic modes (#10) run in parallel on their own
branch once Phase 1 has merged.
**Follows:** [CONSTITUTION.md](../CONSTITUTION.md).
**Branch:** `feat/multi-stream`.

## Goal

Ten people in a room talk over voice. Any of them can start a stream (up to
`maxStreams`, default 4). Anyone else can watch one stream at a time and switch
freely. Each stream has its own supernode tree and failover. Voice keeps
running through all of it.

**Done when** the acceptance tests in [Phase 5](#phase-5-acceptance-tests-and-measurements)
pass and their measured numbers are recorded in the spec and the README.

## Not in this plan

The `cast agent` command (#1), stable `clientId` and reconnect (#4, #5), mic
modes (#10), pause and game-audio toggles, the degraded tiers (#20, with the
tree election), and the moderation methods. Streams are identified by the
streamer's **peer ID** until `clientId` exists.

**Links stay per tree.** The [mesh](mesh.md) (one connection per pair) would
later replace how Phase 4 builds video links, behind the same
`reconcileLocked` entry point. This plan doesn't wait for it: the mesh needs
real-hardware measurements first, and Phase 4 reuses today's link code.

## Where the code is today

| Area | Today | File |
|---|---|---|
| Room | One `streamer`, one `host`/`backup`/`epoch`/`suspects` per room; a second streamer is refused at join | [signaling/server.go](../cast/internal/signaling/server.go) (`room`, `addPeer`) |
| Health | `monitor` → `checkHost(room)` judges one host per room | `server.go` (`monitor`, `checkHost`) |
| Topology | One broadcast per room: `Streamer`, `Host`, `Backup`, `Viewers` (every non-streamer), `Voice` | [proto/proto.go](../cast/internal/proto/proto.go) (`Topology`), `server.go` (`reconcile`) |
| Role | Fixed at join (`Join.Role`); joining as viewer means watching | `proto.Join`, [peer/client.go](../cast/internal/peer/client.go) (`New`, `Run`) |
| Election | Order: current host → **streamer** → best viewer → degraded | [signaling/election.go](../cast/internal/signaling/election.go) (`Elect`) |
| Client links | `reconcileLocked` derives send/recv links from one topology | `client.go` (`reconcileLocked`) |
| Sidecar | Game mode fixed by `--game` at start; stdin knows `mute`, `unmute`, `quit` | [voice/src/main.rs](../voice/src/main.rs), [engine.rs](../voice/src/engine.rs) |

**Key simplification.** With D6 (one stream watched at a time) and D8 (no
watching while streaming), every peer is in **at most one tree**: its own
stream as streamer, or one stream as watcher. So the client's link logic can
stay per-tree: it picks "my stream" out of the room topology and runs the
existing `reconcileLocked` on it. Links don't need to be keyed by stream.

## Design choices

1. **One room-wide topology broadcast** carrying a list of streams, not a
   per-peer message. Rooms are about 10 peers, so the message stays small, and
   every client needs the full list for the stream picker anyway. `Version`
   stays room-wide, so out-of-order handling is unchanged.
2. **Per-stream state moves into a `stream` struct** (host, backup, epoch,
   suspects, stallSince, bitrate). `checkHost` and the election take a
   `*stream`; their logic doesn't change.
3. **Watchers are explicit.** The server keeps `peer.watching` (a streamer ID
   or empty, plus the `video` and `gameAudio` flags). A stream's viewers are
   exactly the peers watching it. Only video watchers are host candidates and
   count for the single-watcher rule; an audio-only watcher is a child in the
   tree that gets game audio only (128 kbps, not a video copy).
4. **New client → server messages** `stream` (start or stop) and `watch`
   (streamer ID and flags, or empty to stop), answered with a non-fatal
   `result` that echoes a client-chosen `ref` and may carry an error type such
   as `stream_limit`. Today `error` is fatal and ends the client.
5. **Server-side cleanup.** When a stream ends (stop, leave, timeout) the
   server clears `watching` on its watchers. When a watcher starts a stream,
   the server clears its `watching` (D8).
6. **Sidecar game mode follows the client's state**: streaming → `reference`,
   watching → `play`, neither → `off`. The mic delay follows the mode unless
   `--mic-delay` was given explicitly.
7. **Expand, migrate, contract.** Phase 3 adds the new protocol **beside** the
   old one, so every commit builds and the existing integration tests check
   the new server unchanged; Phase 4 moves the client over; the old fields
   go in a final commit.
8. **Room cap (D11).** `maxMembers` sits next to `maxStreams`; a join beyond
   it gets `room_full`.

## Phases

Each phase is one or more commits on the branch, green before the next starts.

### Phase 1: live game mode in the sidecar (#9)

Independent of everything else, and it removes the only reason voice could
drop.

- `voice/src/main.rs`: stdin command `game play|reference|off`.
- `engine.rs`: `Engine::set_game_mode(mode)`:
  - resets the game jitter buffer (its start depth differs by mode);
  - resizes `mic_line` to the mode's default delay unless an explicit
    `--mic-delay` was given (padding with silence when growing, dropping the
    oldest samples when shrinking: a glitch of at most 80 ms in the outgoing
    voice, once).
- `peer/voice.go`: `voiceEngine.setGameMode(mode)`, sending the command.
- **Tests:** Rust unit test that switching `Play → Reference` keeps the echo
  canceller working (reuse `streamer_echo_sent`) and that `Off` drops game
  packets; Go test that the sidecar PID is unchanged across mode switches.

Commits: `feat(voice): switch game mode at runtime`, `feat(peer): drive the
sidecar's game mode`.

### Phase 2: election order (#8)

- `Elect`: move the "streamer serves directly" step after the viewer
  candidates. Order becomes: one viewer → direct; current host if it still
  qualifies; best qualifying viewer; streamer if it qualifies; degraded.
- `ElectPair` needs no change: the backup election uses the same order.
- **Tests:** update `TestElect` and `TestElectPair`; add a case where both
  the streamer and a viewer qualify and the viewer wins.
- **Check:** the failover tests give the streamer 10 Mbps, too little to
  host, so viewers already host there and their topologies don't change. If
  another test assumed the streamer hosts, fix the test, not the order.

Commit: `feat(signaling): make the streamer the last choice as supernode`.

### Phase 3: several streams on the server (#6, #7 server side)

- `proto` (additive; nothing is removed in this phase):
  - New types `stream` (`{action: "start"|"stop", bitrateKbps}`), `watch`
    (`{streamer, video, gameAudio}`), `result` (`{ref, error}`); `Message.Ref`.
  - `Topology`: `Streams []StreamTopology` (`Streamer`, `Host`, `Backup`,
    `Watchers` with their flags, `BitrateKbps`, `Epoch`, `Degraded`, `Reason`),
    `MaxStreams` and `MaxMembers`; `Voice` and `Names` stay room-wide.
  - `Join.Role` and the single-stream `Topology` fields **stay** for now.
- **Compatibility shim** in `server.go`: a join with `Role: streamer` is an
  automatic `stream start`; `Role: viewer` is an automatic `watch` of the only
  stream, or of the first one that starts. The single-stream fields are filled
  from that stream. Every existing integration test runs unchanged against
  the new server.
- `server.go`:
  - `room.streams map[string]*stream`; `peer.watching`, `peer.streaming`.
  - `stream start`: refuse with `stream_limit` (and the current streamers) at
    `maxStreams`, or `already_streaming`; clear the peer's `watching`.
  - `stream stop` / leave: delete the stream, clear its watchers.
  - `watch`: `unknown_stream` if not live, `watch_while_streaming` if the
    peer streams, invalid if both flags are false; otherwise set `watching`.
    A `watch` for the stream already watched only changes the flags.
  - Join: `room_full` at `maxMembers`.
  - `reconcile`: elect per stream from that stream's watchers; bump that
    stream's epoch only when its host or streamer changes.
  - `monitor`: loop over rooms, then streams; `checkHost(stream)` counts only
    watchers whose status upstream is that host.
- `cmd/cast`: `signal -max-streams 4 -max-members 10`.
- **Tests** (new `server_test.go`, WebSocket clients against `httptest`):
  limit reached → `stream_limit` with the streamer list; the eleventh join →
  `room_full`; watch switches move the peer between `Watchers` lists; an
  audio-only watcher is never elected host; a host failure in stream A
  changes A's epoch and not B's; stopping a stream empties its watchers; the
  shim maps `Role` joins onto streams. The existing `peer` integration tests
  pass unchanged.

Commits: `feat(proto): describe several streams per room`,
`feat(signaling): run one tree per stream with a room stream limit`,
`feat(signaling): cap rooms at maxMembers`,
`feat(cli): add -max-streams and -max-members to the signal server`.

### Phase 4: streaming and watching as client state (#6, #7 client side)

- `peer.Client`:
  - Create both the streamer tracks (`sample`, `idle`, `audio`, `audioIdle`)
    and the forwarding tracks (`fwd`, `fwdAudio`) up front; drop the
    role-based split in `New`.
  - `StartStream(StreamConfig{Source, Audio, BitrateKbps, FPS, Pace})` and
    `StopStream()`: send the `stream` message, wait for its `result`, start or
    stop `pumpFrames` / `pumpAudio` (with their own cancel func), set the
    sidecar game mode.
  - `Watch(streamer, video, gameAudio)` / `Unwatch()`: send `watch`, wait for
    `result`, set the sidecar game mode.
  - A parent keeps its video sender on the idle track for an audio-only
    child (the same swap the backup uses).
  - `applyTopology`: pick my stream (the one I stream, or the one I watch)
    and pass it to `reconcileLocked`, which keeps its current logic with
    `Viewers` = that stream's watchers. No stream → no video links.
  - Expose the stream list to the UI hook (`OnTopology`).
- `cmd/cast`:
  - `cast stream` = join, then `StartStream`; exits as today when the source
    ends.
  - `cast watch -stream X`: watch that streamer, where X is a peer ID or a
    name; a name shared by several streamers exits with the matches and their
    IDs. Without `-stream`, watch the only live stream and wait for one if
    none is live; if several are live, list them and exit with an error.
    `-audio-only` sets `video: false`.
- **Tests:** existing failover tests still pass unchanged in behaviour (they
  now call `StartStream` / `Watch`); a client stops streaming and its voice
  links stay connected.

Commits: `feat(peer): make streaming and watching runtime state`,
`feat(cli): pick a stream to watch`.

### Phase 5: acceptance tests and measurements

Integration tests in `peer/`, each mirroring a line of the scenario:

| Test | Asserts | Records |
|---|---|---|
| `TestTwoStreamsIndependent` | Stream B's viewers report no `stalled` status while stream A's host hangs and fails over | A's failover time (expect ≈570 ms, as today) |
| `TestStreamLimit` | The fifth `StartStream` fails with `stream_limit` and lists four streamers | — |
| `TestSwitchStreams` | A watcher moves from A to B; A's watcher count drops; B delivers frames | **Time from `Watch(B)` to the first frame** (fills the "not measured" in the spec) |
| `TestSwitchAwayFromHost` | A's host switches to B; A's backup takes over | A's gap (expect ≈20 ms, the planned-leave path) |
| `TestVoiceSurvivesStreams` | With the real sidecar: start, stop, switch and unwatch streams while voice runs; same sidecar PID, no voice gap over 100 ms | Longest voice gap |
| `TestStreamerHostsLast` | 3 watchers, a qualifying watcher and a qualifying streamer: the watcher hosts | — |

Then update:

- `cast/AGENT_PROTOCOL.md`: the measured switch time; change items #6–#9
  marked done.
- `cast/README.md`: architecture and failover sections for several streams,
  the new flags, the measured numbers.

**Contract:** remove `Join.Role`, the single-stream `Topology` fields and the
shim, now that nothing uses them.

Commits: `test(peer): cover several streams, switching and voice continuity`,
`refactor(proto): remove the single-stream fields and the role shim`,
`docs: document several streams per room`.

## Risks

| Risk | Mitigation |
|---|---|
| A watcher with little upload ends up the only qualifying host and every tree gets slower | The election already falls back to the streamer and then degraded mode; covered by the existing degraded tests. The degraded tiers (D14) come later, with the tree election |
| The compatibility shim hides a behaviour change | The shim only exists between Phases 3 and 5; Phase 5's tests use the new messages, and the contract commit deletes it |
| Topology messages grow with every stream | 10 peers and 4 streams is a few kilobytes; measure in `TestTwoStreamsIndependent` and revisit only if it matters |
| Two quick switches race: an offer for the old tree arrives after the new topology | Already handled: stale link IDs are ignored and pending offers are dropped when their sender isn't in my tree (the same rule as voice) |
| Mic delay change on mode switch clips a syllable | One glitch of at most 80 ms per switch; documented; only when the delay is the automatic one |
| Integration tests get slower with more peers | Keep each new test to the fewest peers that show the behaviour (2 streams, 2 watchers each) |

## Order and size

| Phase | Depends on | Size |
|---|---|---|
| 1. Sidecar game mode | — | Small |
| 2. Election order | — | Small |
| 3. Server | 2 | Medium |
| 4. Client and CLI | 1, 3 | Medium |
| 5. Acceptance and docs | 4 | Medium |

Phases 1 and 2 can be done in either order or in parallel.
