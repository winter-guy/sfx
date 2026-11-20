# A connection between every pair of members

**Status:** proposal. Decided in
[the spec](../cast/AGENT_PROTOCOL.md#decisions): **room cap 10** (D11),
**"hide my IP"** with a TURN server built into `cast signal` (D12,
[cost](#hide-my-ip-what-it-costs)). **Not adopted:** the slower keepalive on
battery ([why](#keepalive-and-battery)). The mesh itself waits on
real-hardware measurements. Answers open question 2 of
[supernode-election.md](supernode-election.md#open-questions).
**Question:** in a room of 10, should every pair of members keep one
WebRTC connection open all the time, carrying voice plus an idle video track
and an idle game-audio track in each direction? Tree changes would then be
track swaps instead of new connections.

---

## Contents

- [Summary](#summary)
- [Measured cost](#measured-cost)
- [What a pair can carry](#what-a-pair-can-carry)
- [Every member state](#every-member-state)
- [Room combinations](#room-combinations)
- [Events over time](#events-over-time)
- [Design options](#design-options)
- [Downsides](#downsides)
- [Room size](#room-size)
- [Hide my IP: what it costs](#hide-my-ip-what-it-costs)
- [Keepalive and battery](#keepalive-and-battery)
- [Recommendation](#recommendation)
- [Open questions](#open-questions)

---

## Summary

- **Cheap.** Measured: a full 10-member mesh costs about **2.2 MB of memory
  and 0.7% of one CPU core per member** while idle. Adding the idle video and
  game-audio tracks to the voice connections we already have costs **+0.6 MB
  and about +0.1% of a core per member**.
- **Simpler state.** With a mesh, the connection count is fixed at 45
  whatever people do. Without it, the count swings from 0 to 63 and every
  watch, switch, stream start or relay change opens or closes connections.
- **Faster.** Watching, switching streams, starting a stream and replacing a
  relay become track swaps plus a keyframe, with no SDP exchange and no new
  connection.
- **A pair never carries video both ways at once.** That's provable from D6,
  D8 and seed-only pure seeders, so one video and one game-audio track per
  direction is always enough, and the connection never needs renegotiating
  after setup.
- **The real cost is privacy:** every member learns every other member's IP
  address, the streamer's included. The fix is a per-member "hide my IP"
  option (TURN only), not a smaller mesh.
- **Decided:** rooms capped at **10 members** (D11); "hide my IP" with a TURN
  server built into `cast signal` (D12). **Proposed:** the full mesh, once
  measured on real hardware. **Not adopted:** a slower keepalive on battery.

---

## Measured cost

A throwaway program (all members in one process, loopback network, Pion
v4.2.27, the same codecs, ICE settings and default interceptors as `cast`:
1 s keepalive) built every pair's connection and measured heap, goroutines and
CPU over 30 s per phase. Per-member figures are totals divided by 10.

| Mesh | State | Heap per member | Per connection | Goroutines per connection | CPU per member (one core) |
|---|---|---|---|---|---|
| Voice track only (today's voice mesh) | Idle | 1.64 MB | 186 KB | 16 | 0.63% |
| Voice track only | Everyone's voice live | 1.82 MB | 208 KB | 16 | 3.30% |
| **Voice + idle video + idle game audio** | Idle | **2.20 MB** | 250 KB | 18 | **0.72%** |
| **Voice + idle video + idle game audio** | Everyone's voice live | **2.50 MB** | 285 KB | 18 | **3.31%** |

All 45 connections came up in about 30 ms on loopback. Over the internet,
expect each to take one STUN gathering plus a few round trips (hundreds of
milliseconds), in parallel.

**Reading the numbers:**

- The idle extra tracks cost **≈ 0.6 MB and ≈ 0.1% of a core per member**
  (close to run-to-run noise: an earlier run without interceptors showed
  0.60% vs 0.65%).
- For a member **not** in voice, the mesh costs the full 2.2 MB and 0.7% of
  a core. That's the price of having every edge ready.
- Live voice (50 packets per second to and from 9 peers, encrypted) costs
  about 3.3% of a core per member. Today's voice mesh already pays that.
- Keepalive traffic isn't measured here. A STUN check plus its reply is
  about 250 bytes, once per second per pair, so roughly 2 kbps per pair and
  under 20 kbps per member. Negligible beside one 10.6 Mbps video copy.

**Caveats:** one machine, loopback, no real NAT or Wi-Fi. Treat these as
order-of-magnitude figures, and repeat the measurement on a Windows gaming PC
before relying on it.

---

## What a pair can carry

For a pair `{u, v}`, each direction has three tracks: voice, video, game
audio. Video and game audio always move together, so call them "media".

**Claim: media flows in at most one direction of a pair at any time.**

- `u → v` media means `u` is `v`'s parent in some stream's tree. So `v`
  watches that stream, and `u` either streams it or watches it too.
- `v → u` media at the same time would make `v` the parent of `u` in the
  same stream, because each member is in at most one tree: as streamer, as
  watcher (D6, D8), or as seed-only relay (pure seeding is limited to members
  who watch nothing). A parent of your parent can't also be your child, so
  that's a cycle, which a tree can't have.

So each pair is in one of **six states**:

| Voice | Media | Example |
|---|---|---|
| Off | None | Two members not in voice, neither feeding the other |
| Off | `u → v` | `u` relays the stream to `v`; neither is in voice |
| Off | `v → u` | The same, reversed |
| On | None | Two people talking, watching nothing or different streams |
| On | `u → v` | Talking, and `u` is `v`'s parent |
| On | `v → u` | The same, reversed |

Times two for how the pair connects (direct, or through TURN). Across the
room: the number of media flows equals the number of watchers (each watcher
has exactly one parent), so **at most 9 of the 45 pairs ever carry media**.
The other 36 or more carry voice or nothing.

---

## Every member state

What one member's 9 connections are doing, for every kind of member:

| Member | Voice | Media in | Media out | Connections in use |
|---|---|---|---|---|
| In the lobby (not watching) | Off | 0 | 0 | None (all 9 idle) |
| In the lobby | On | 0 | 0 | Voice on each pair with a voice member |
| Watching, leaf | Off | 1 | 0 | 1 |
| Watching, leaf | On | 1 | 0 | 1 + voice |
| Watching, relaying (seeding) | Off | 1 | `k` ≤ its copies | 1 + `k` |
| Watching, relaying | On | 1 | `k` | 1 + `k` + voice |
| Streaming | Off | 0 | 1 (2 if allowed) | 1–2 |
| Streaming | On | 0 | 1–2 | 1–2 + voice |
| Seed-only relay (watches nothing, relays one stream) | Off / On | 1 | `k` | 1 + `k` (+ voice) |

The connection count doesn't depend on the row: it's always 9.

---

## Room combinations

Counting every combination is pointless: each member can be in about a
dozen states, so a room of 10 has on the order of 10¹³ states. But with a
mesh, the cost depends on just three numbers:

```
connections         = n(n − 1) / 2            = 45, always
live media flows    = watchers                (≤ 9)
live voice flows    = v(v − 1)                (v = members in voice)
```

**Today's design** (voice mesh plus separate video connections) instead
needs, per stream with `w` watchers: `w` connections when `w ≤ 1`, otherwise
`2w` (streamer to host and backup, host to every other watcher, backup to
every other watcher on standby):

```
connections (today) = v(v − 1)/2 + Σ over streams of (w ≤ 1 ? w : 2w)
```

**Representative rooms of 10:**

| # | Room | Voice | Streams (watchers) | Today | Mesh | Live media flows |
|---|---|---|---|---|---|---|
| 1 | Lobby, nobody talking | 0 | — | 0 | 45 | 0 |
| 2 | Hanging out in voice | 10 | — | 45 | 45 | 0 |
| 3 | Watch party, everyone talking | 10 | 1 (9) | 63 | 45 | 9 |
| 4 | Watch party, silent | 0 | 1 (9) | 18 | 45 | 9 |
| 5 | One friend watching one streamer | 10 | 1 (1) | 46 | 45 | 1 |
| 6 | Two streams | 10 | 2 (4 + 4) | 61 | 45 | 8 |
| 7 | Four streams (the cap) | 10 | 4 (2 + 2 + 1 + 1) | 55 | 45 | 6 |
| 8 | Five duos | 10 | 5 (1 each) | 50 | 45 | 5 |
| 9 | Half in voice, two streams | 5 | 2 (4 + 4) | 26 | 45 | 8 |
| 10 | Half in voice, small stream | 5 | 1 (3) | 16 | 45 | 3 |

**What the table says:**

- **When everyone is in voice (rows 2–8), which is the common case for a
  group of friends, the mesh has *fewer* connections than today**, because
  voice and video share one connection per pair.
- The mesh costs more only when few people are in voice (rows 1, 4, 9, 10).
  The extra is at most 45 idle connections, which at 2.2 MB and 0.7% of a
  core per member is small.
- **Per member**, today's design is lopsided: a host in row 3 holds 19
  connections (9 voice, 1 in, 8 out, 1 standby in). In the mesh, everyone
  holds 9.

---

## Events over time

| Event | Mesh | Today |
|---|---|---|
| A member joins | 9 new connections, in parallel | Voice connections to the voice members; nothing for video until it watches |
| Starts watching | Parent swaps its idle track to live; one keyframe | 1–2 new connections (parent, plus the backup's standby) |
| Switches stream | Old parent goes idle, new parent goes live; one keyframe | Close 1–2, open 1–2 (about a second, not measured) |
| Starts streaming | Streamer's link to its first relay goes live; immediate | New connections to host and backup |
| A relay fails | The server picks new parents; they swap tracks | Only the host has a warm backup; other relays would need new connections |
| A member leaves | 9 connections close | Its links close; trees re-elect |
| Network change (Wi-Fi to wired) | 9 ICE restarts | ICE restarts on voice links; media links rebuilt |
| Latency data for the election | Every pair, every second, free | Only between members that happen to be connected |
| Becomes a seeder | Nothing to set up | Links appear when it's given children |

---

## Design options

| Option | Connections (row 3 / row 4 / row 1) | Switching | Relay failover | Latency data | IP exposure | Complexity |
|---|---|---|---|---|---|---|
| **A. Today:** voice mesh, video on demand | 63 / 18 / 0 | New connection | Host only (warm backup) | Partial | Voice members; streamer to its host and backup | Two link systems |
| **B. Full mesh, one connection per pair** (voice + idle media) | 45 / 45 / 45 | Track swap | Track swap | Full | Everyone | One link system, never renegotiated |
| C. Full mesh, two connections per pair (voice, media) | 90 / 45 / 45 | Track swap | Track swap | Full | Everyone | Two connections per pair; voice isolated from media problems on the same path |
| D. Voice mesh + latency-only probes, media on demand | 63+ / 45+ / 45 | New connection | As today | Full | Everyone | Three kinds of connection |
| E. "Useful" mesh: a pair connects only if both are in voice, or one can send (streams or seeds) | Varies with every voice, seeding or streaming toggle | Track swap | Track swap | All the election needs | Fewer | Connections appear and vanish as settings change |
| F. Lazy: connect on demand, keep for N minutes | Grows to B over time | Swap if cached | Partial | Partial | Grows to everyone | Cache rules |

**On E:** the election only needs latencies on edges that touch a possible
sender (streamer → relay, relay → watcher). Leaf-to-leaf latencies are never
used. So E gives the election everything it needs with fewer connections.
But in the common case (everyone in voice), E is already the full mesh, and
in the other cases it saves at most a few MB while adding churn every time
someone toggles voice or seeding. Not worth it at 10 members.

**On C:** separate connections would keep a media problem on a pair (say,
an ICE restart) from touching voice on that pair. But both share the same
network path anyway, and with option B nothing renegotiates after setup, so
there's little to isolate. It doubles the connection count for no measured
gain.

---

## Downsides

1. **Everyone sees everyone's IP address.** WebRTC peers learn each other's
   public IP (and, with host candidates, LAN addresses). Today that's already
   true between voice members; the mesh extends it to all members, including
   the streamer. Gaming streamers get DDoSed, so this matters.
   - **Mitigation:** a per-member **"hide my IP"** setting: that member uses
     TURN only (ICE relay policy), so peers see the TURN server's address.
     Cost: all of that member's media goes through TURN, so for a streamer
     one 10.6 Mbps copy, for a relay every copy it sends.
   - The election treats a hidden member's edges as TURN edges (higher
     latency), so it's rarely picked as a relay.
2. **Grows as n².** Fine at 10 (45 pairs). See [Room size](#room-size).
3. **Laptop power.** 0.7% of a core sounds small, but it's constant
   wake-ups from keepalives and timers on 9 connections. Worth measuring on
   battery. A slower keepalive was considered and
   [not adopted](#keepalive-and-battery).
4. **Join cost.** A new member opens 9 connections at once. Each one
   gathers candidates and does a DTLS handshake. In parallel that's fine, but
   on a slow machine or bad network the member's edges come up one by one.
   Rule: **an edge is usable by the election only once it is connected and
   has a measured RTT.**
5. **TURN allocations are held all the time** for pairs that need TURN. Each
   allocation is small on the server, and refreshes are infrequent, but a
   self-hosted TURN server should be sized for `members × peers`
   allocations.
6. **One more thing that can fail per pair.** A pair whose ICE fails can't
   carry media between those two; the election simply avoids that edge. That
   is better information than today, where the failure shows up only when a
   video link is attempted.

---

## Room size

**Decided (D11): 10 members per room.** For reference, scaling the measured
per-connection cost (≈ 250 KB and ≈ 0.08% of a core per idle connection;
≈ 0.37% per live voice peer):

| Members | Pairs | Connections per member | Idle mesh per member | Live voice per member |
|---|---|---|---|---|
| 5 | 10 | 4 | ≈ 1.0 MB, 0.3% | ≈ 1.5% |
| **10 (the cap)** | 45 | 9 | **≈ 2.2 MB, 0.7%** (measured) | **≈ 3.3%** (measured) |
| 16 | 120 | 15 | ≈ 3.7 MB, 1.2% | ≈ 5.5% |
| 25 | 300 | 24 | ≈ 6 MB, 1.9% | ≈ 8.8% |
| 50 | 1225 | 49 | ≈ 12 MB, 3.9% | ≈ 18% |

Live voice here is only the transport (encryption, sockets). The sidecar
also decodes and mixes every speaker, and each member uploads its voice to
every peer (about 50 kbps each). **The voice mesh runs out before the idle
mesh does**: at 25–50 people voice needs a mixing server anyway, and the
whole peer-to-peer design changes. The cap of 10 keeps both well inside the
measured range, and matches the capacity maths the rest of the design uses.

The server enforces the cap at join with a `room_full` error. `maxStreams`
stays at 4, which is below the ⌊10/2⌋ = 5 ceiling.

---

## Hide my IP: what it costs

**What it does.** A hidden member's connections use only TURN relay
candidates (`ICETransportPolicy: relay` in Pion). Its SDP contains no host or
server-reflexive candidates, so peers only ever see the TURN server's
address. The signaling server and the TURN operator still see the member's
real IP.

**It has to start at join.** The mesh connects everyone as soon as they
join, so a member who starts streaming later has already revealed their IP.
Switching to TURN at `stream.start` would mean an ICE restart on all 9 pairs:
a voice glitch on every pair (against D9), and too late anyway. So "hide my
IP" is a **per-member setting applied at join**:

- the app turns it on by default for anyone who has streamed before, and
  asks on someone's first `stream.start` ("Hide your IP from now on? Takes
  effect next time you join, or now with a short voice reconnect");
- the app remembers the answer, like the mic mode.

**What it costs, per hidden streamer:**

| Cost | Amount | Notes |
|---|---|---|
| A TURN server | Decided (D12): `cast signal -turn` runs one in the signaling binary (`pion/turn`), handing out short-lived credentials in `welcome`. Today there is only a public STUN server | Whoever runs the signaling server pays the traffic below; UDP 3478 and a relay port range must be open. A second server in another region for redundancy; `network.ice` can point elsewhere |
| TURN traffic, video | **≈ 3.7 GB per hour** in and the same out, per copy (8.1 Mbps) | Budget 2 copies → double |
| TURN traffic, voice | ≈ 0.2 GB per hour each way | Voice to and from 9 peers at ≈ 50 kbps each |
| Total TURN egress | **≈ 4 GB per streamer-hour** (1 copy) | 4 hidden streamers ≈ 16 GB per hour and ≈ 36 Mbps sustained through the server |
| Money | Hyperscaler egress at roughly $0.05–0.09 per GB ≈ **$0.20–0.37 per streamer-hour**; a VPS with included traffic: a fixed monthly price, about 240 streamer-hours per TB | Approximate list prices; check the provider |
| Latency | The detour via TURN on the **root edge** (streamer → first relay) and on the streamer's voice: **+2–10 ms** with a TURN server in the same region, **+30–100 ms** with one far away | Every viewer of that stream pays it, because it's on the root edge. TURN servers near the users matter |
| Reliability | A TURN outage cuts the hidden streamer's stream and voice | Two TURN servers; ICE picks the one that works |
| Client CPU and bandwidth | Negligible: ≈ 4 bytes of TURN framing per packet (≈ 0.3%) | — |
| Election | Nothing to change: the streamer's RTTs are measured through TURN, so the detour is already in the latency matrix | — |

**What it doesn't hide:** the streamer still sees viewers' IPs, and viewers
still see each other's. Hiding is per member; anyone can turn it on, at the
same TURN cost for the copies they send.

**Without a TURN server**, a join with `hideIp` fails with `turn_unavailable`;
the core never falls back to exposing the address.

**LAN addresses.** mDNS candidates would hide LAN addresses, but `cast`
disables mDNS on purpose (`peer/media.go`): it fails on networks that block
multicast, which would push friends on the same LAN onto hairpin NAT paths
many routers don't support. A private LAN address reveals little. Host
candidates stay.

---

## Keepalive and battery

**Outcome: not adopted.** The keepalive stays at 1 s everywhere. The idea is
parked in [later.md](later.md), behind a battery measurement that shows real
drain. Why, in short:

- `cast` builds every connection with ICE timeouts of 3 s (disconnected) and
  6 s (failed) (`SetICETimeouts` in `peer/media.go`). With a 15 s keepalive,
  an idle pair hears nothing for 15 s and is declared failed, the same
  problem as switching it off. Slowing it means raising those timeouts on
  those connections too, so a dead idle pair is noticed after about 45 s.
- It only saves anything when **both** ends are on battery: a peer on a 1 s
  keepalive still wakes the battery side every second.
- The measured gain is 0.17% of a core per member, for three new mechanisms
  (per-connection intervals, per-connection timeouts, rebuilding idle pairs
  on a power change).

The analysis that led there follows.

**Original proposal:** turn the keepalive off on battery, back on when
plugged in; not for ARM laptops, whose efficiency cores make it cheap.

**What the keepalive does** (checked in `pion/ice` v4.4.9): every interval,
each side sends a STUN request on the pair it uses, whether or not other
traffic flows. That one packet does three jobs:

1. **Consent:** WebRTC endpoints must stop sending to a peer that hasn't
   confirmed consent within 30 s (RFC 7675).
2. **NAT bindings:** home routers drop idle UDP mappings, often after
   30 s to a few minutes. Once dropped, the pair is dead until an ICE restart.
3. **RTT:** it's how the election's latency matrix stays fresh.

**So it can't be switched off.** With the keepalive off, an idle pair (no
voice, no media) receives nothing, Pion declares it disconnected after our
3 s timeout and failed after 6 s, and the mesh's whole point (edges ready to
use) is gone. It can only be **slowed down**, to at most about 15 s so it stays
inside the 30 s consent window and typical NAT timeouts.

**What slowing it saves** (measured, idle 10-member mesh, per member):

| Keepalive | CPU (one core) |
|---|---|
| 1 s (today) | 0.72% |
| 5 s | ≈ 0.61% (run without interceptors) |
| 15 s | 0.55% |

The keepalive accounts for **≈ 0.17% of one core**, about a quarter of the
idle mesh. The rest is Pion's other per-connection timers. Worth having on a
battery, but small.

**Two complications:**

- **Pion can't change it on a live connection.** The interval is fixed when
  the ICE agent is created (`WithKeepaliveInterval` returns
  `ErrAgentOptionNotUpdatable` afterwards). Options:
  1. choose it when each connection is created, from the power state then;
  2. on a power change, rebuild only the pairs carrying nothing (no voice,
     no media), which nobody notices; leave busy pairs as they are;
  3. contribute a small change to Pion to allow updating it at runtime.

  Proposed: 1 and 2 now, 3 if it proves worth it.
- **Slower keepalive doesn't slow media failover.** Failover is driven by
  the watchers' 250 ms status heartbeat and the 400 ms stall rule, not by
  ICE. What gets slower: noticing that an **idle** pair died (up to about a
  minute), and RTT freshness (every 15 s instead of every second, which is
  plenty for an election).

**"Not for ARM laptops":** agreed in spirit, but the right test is "has
efficiency cores", not "is ARM". Apple Silicon and Snapdragon X have them,
and so do Intel 12th-gen-and-later and Core Ultra laptops. Detection:
`sysctl hw.nperflevels` > 1 on macOS, `GetSystemCpuSetInformation`
(`EfficiencyClass`) on Windows, `cpu_capacity` in sysfs on Linux. Even with
efficiency cores, each wake-up keeps the chip out of its deepest idle state
briefly, so the saving isn't zero, just smaller.

**Rule:**

| Power | Efficiency cores | Keepalive |
|---|---|---|
| Plugged in | Any | 1 s |
| Battery | Yes | 1 s |
| Battery | No | 15 s |
| Desktop (no battery) | Any | 1 s |

---

## Recommendation

1. **Option B:** one connection per pair for every member, with three
   transceivers per direction (voice, video, game audio), negotiated once at
   join and never renegotiated. Video and game audio idle until the tree says
   otherwise.
2. **Decided (D11): a room cap of 10 members**, enforced by the server
   (`room_full`).
3. **Decided (D12): "hide my IP"**, as a per-member setting applied at join:
   on by default for anyone who has streamed before, asked on a first
   stream, remembered by the app. The TURN server is built into
   `cast signal` ([cost](#hide-my-ip-what-it-costs)).
4. **Keepalive: 1 s everywhere.** The battery variant is not adopted
   ([why](#keepalive-and-battery)).
5. **Usable edges only:** the election uses an edge once it's connected and
   has an RTT.
6. **Measure on real hardware** before building: the same program on a
   Windows gaming PC and a laptop on battery, and a 10-machine test over real
   networks for join time.

**How this changes the plans:**

- [multi-stream.md](multi-stream.md) doesn't wait for the mesh: its Phase 4
  keeps per-tree connections. If the mesh is adopted, it replaces how video
  links are built (swap tracks instead of opening connections), behind the
  same `reconcileLocked` entry point, and the voice link code
  (`peer/voice.go`) becomes the general mesh.
- [supernode-election.md](supernode-election.md): failover design B is this
  mesh; open question 2 is answered here.

---

## Open questions

1. ~~Who runs TURN?~~ **Decided (D12):** the signaling server
   (`cast signal -turn`); whoever runs it pays the ≈ 4 GB per streamer-hour.
2. ~~Upstream Pion change for a runtime keepalive interval~~ Moot: the
   battery keepalive is not adopted.
3. **Room-full behaviour:** refuse the eleventh member (`room_full`, as
   decided for now), or later let them wait in a queue?
