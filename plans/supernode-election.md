# Supernode election: research and proposal

**Status:** brainstorm, for discussion. Nothing here is decided until it is
moved into [cast/AGENT_PROTOCOL.md](../cast/AGENT_PROTOCOL.md#decisions) as a
decision.
**Scope:** how each stream's tree is built (who relays to whom), what each
node measures and when, and how members control what they share.

---

## Contents

- [Summary](#summary)
- [What the election does today](#what-the-election-does-today)
- [The problem as math](#the-problem-as-math)
- [Capacity: turning upload, CPU and seeding into copies](#capacity-turning-upload-cpu-and-seeding-into-copies)
- [Latency: where the numbers come from](#latency-where-the-numbers-come-from)
- [Several supernodes per stream: a worked example](#several-supernodes-per-stream-a-worked-example)
- [Protecting the streamer](#protecting-the-streamer)
- [Seeding](#seeding)
- [The check: what to measure and when](#the-check-what-to-measure-and-when)
- [Failover with many relays](#failover-with-many-relays)
- [Stability](#stability)
- [The algorithm](#the-algorithm)
- [More ideas](#more-ideas)
- [Arguments on the proposal](#arguments-on-the-proposal)
- [What would change](#what-would-change)
- [Open questions](#open-questions)
- [Sources](#sources)

---

## Summary

1. **Build a tree, not a star.** Each stream gets a small tree of relays
   (depth 2, at most 3) instead of one supernode serving everyone. At our size
   (≤ 10 people) the best tree can be found **exactly**, in microseconds.
2. **Optimise latency under capacity limits.** Every node's resources become
   one number, the **copies it can send**. The election then minimises the
   worst viewer's latency without exceeding anyone's copies. No weighted
   scores mixing Mbps, CPU % and milliseconds.
3. **Latency is free.** Every pair of peers already has an ICE connection
   (the voice mesh), and ICE measures its round-trip time every 2 s. Extend
   the mesh to every member and the full latency matrix costs nothing.
4. **The streamer sends one copy by default** and relays nothing else. It
   may allow a second copy, which the worked example shows can cut the worst
   viewer's latency by 20%.
5. **Seeding is an explicit, capped opt-in:** `off`, or `on` with an upload
   cap in Mbps (shown as "≈ N viewers at 1080p60" and "≈ X GB per hour").
   The cap is enforced by the shape of the tree, never by dropping packets.
6. **Measure passively; probe rarely and precisely.** The 250 ms heartbeat
   never waits for anything; it carries the latest cached metrics. Live
   traffic and OS counters are read continuously. An active upload probe
   runs only on join, after a network change, and **before promoting a node
   to more copies than it has proven**, over the exact links it would use.
7. **CPU matters little, "is this PC gaming" matters a lot, disk not at
   all.** Forwarding costs about 2.3% of one core per copy (measured), so CPU
   is a gate, not a ranking.
8. **One mesh, many trees.** If every pair keeps one pre-negotiated
   connection with an idle video track, any tree change is a track swap with
   no signaling. Failover then costs detection plus one keyframe.

---

## What the election does today

[`Elect`](../cast/internal/signaling/election.go) picks **one** host per
room: a single viewer gets the stream directly; otherwise keep the current
host if it qualifies, then the streamer, then the best viewer (wired, then
upload, then join order), else degraded. A host "qualifies" when its
self-reported upload ≥ 1.3 × bitrate × copies.

Limits:

| Limit | Consequence |
|---|---|
| One host serves everyone | It needs 10.6 × (n − 1) Mbps; a room without one such node is degraded even if three medium nodes together could serve it |
| No latency input | A wired fiber node in another city wins over a nearby cable node, doubling latency for people near the streamer ([example](#several-supernodes-per-stream-a-worked-example)) |
| Upload is measured once, against the signaling server | It measures the path to the server, not to the peers, and goes stale |
| Nothing about the machine | A PC running a CPU-bound game can be picked to relay |
| `canHost` is a yes/no | A member can't say "share at most 20 Mbps" |

---

## The problem as math

**Inputs** for one stream:

- the streamer `s` and the watchers `W` (`n = |W|`);
- the bitrate `r` (video + game audio) and the headroom `h = 1.3`;
- for each node `i`, its **copies** `c_i`: how many children it may feed
  ([next section](#capacity-turning-upload-cpu-and-seeding-into-copies));
  for the streamer, its budget `c_s` (default 1);
- for each pair, the one-way latency `l_ij ≈ RTT_ij / 2`;
- the forwarding delay `f` of a relay (receive, rewrite, re-encrypt, send:
  around 1–3 ms);
- a penalty `p_i` in milliseconds for unreliable relays (Wi-Fi, battery,
  recent failure).

**Output:** a tree rooted at `s` that spans `W`, where each node has at most
`c_i` children.

**Latency of a watcher** `w` with path `s → k_1 → … → k_m → w`:

```
L(w) = l(s,k_1) + Σ [ f + p(k_j) + l(k_j, k_j+1) ]
```

**Objective**, compared in order (lexicographic):

1. feasible at full quality (everyone served within copies);
2. the **worst** viewer's latency `max_w L(w)`, rounded to 5 ms buckets so
   measurement noise doesn't flip the result;
3. **churn**: how many watchers change parent compared with the current
   tree;
4. the **mean** latency;
5. the number of relays (fewer relays, fewer things that can fail).

**Hardness.** This is the *minimum-diameter, degree-limited spanning tree*
problem studied by Shi and Turner for overlay multicast. It is NP-hard (with
every degree limit at 1, the tree is a path, which is the Hamiltonian path
problem). The standard heuristic is the greedy *Compact Tree* algorithm,
which adds one node at a time at its lowest-latency feasible attachment.

**Why that doesn't hurt us.** NP-hard is about large `n`. With `n ≤ 9`
watchers and depth ≤ 2, the search is small enough to be exact:

- choose which of the streamer's `c_s` children are relays (at most
  C(9,1) + C(9,2) = 54 options for `c_s ≤ 2`);
- assign the remaining watchers to those relays so as to minimise the worst
  latency: binary search over the threshold `T`, and for each `T` check
  whether a **bipartite assignment with capacities** exists (a max-flow on
  ≤ 20 nodes);
- depth 3 (relays feeding relays) only when depth 2 is infeasible, using the
  Compact Tree greedy plus local swaps.

Either way it runs in well under a millisecond on the signaling server.
Past about 30 people, switch to the greedy alone.

**Feasibility, in one line.** With depth ≤ 2, a stream can reach at most
`c_s + Σ (c_k over the streamer's best c_s children)` watchers. If that is
less than `n`, a deeper tree or a lower bitrate is needed.

**The highest bitrate that fits.** For a tree, every node needs
`r ≤ B_i / (h × children_i)`, so the stream's ceiling is

```
r* = min over senders i of  B_i / (h × children_i)
```

where `B_i` is the node's shareable upload. Example: one hub with 50 Mbps
serving 8 others gives `r* = 50 / (1.3 × 8) ≈ 4.8 Mbps`. The server can tell
the streamer "this room can carry about 4.8 Mbps; drop to 900p or 30 fps",
which beats a silent degraded state.

---

## Capacity: turning upload, CPU and seeding into copies

```
copies_i = min( floor( B_i / (h × r) ),  floor( cpuFree_i / k ) )

B_i = min( seedCap_i,  w_i × U_i  −  voiceUp_i )
```

| Symbol | Meaning | Value / source |
|---|---|---|
| `U_i` | Upload capacity estimate | Join probe, then raised by proven sending ([the check](#the-check-what-to-measure-and-when)) |
| `w_i` | Link factor | 1.0 wired, 0.6 Wi-Fi (shared airtime and retransmissions make Wi-Fi relays much worse than their nominal rate) |
| `voiceUp_i` | Voice mesh upload | ≈ 0.5 Mbps in a room of 10 |
| `seedCap_i` | The member's seeding cap | 0 when seeding is off |
| `h` | Headroom per copy | 1.3 (packet overhead, keyframe bursts, retransmissions) |
| `k` | CPU per copy | ≈ 2.3% of one core per 8 Mbps copy (16% for 7 copies at 8.7 Mbps, measured on the dev Mac) |
| `cpuFree_i` | Spare CPU | System idle time, smoothed; 0 while the PC is gaming |

**What the numbers say:**

- **Upload is almost always the binding limit.** A node with half a core
  free could forward about 20 copies by CPU, but few home connections can
  upload 20 × 10.6 Mbps.
- **CPU % is a poor ranking signal** but a good **gate**. What actually hurts
  is relaying on a PC that is running a CPU- or GPU-bound game: even 5% can
  cost frames there. So "gaming" sets `cpuFree = 0` unless the member
  explicitly seeds anyway.
- **Memory is a gate only** (refuse relaying when free memory is very low).
  Forwarding holds a few packets per link.
- **Disk is irrelevant.** Relays never touch disk. Drop it from the check.

---

## Latency: where the numbers come from

| Source | Cost | Accuracy | Verdict |
|---|---|---|---|
| **ICE round-trip time on the mesh**: Pion sends a STUN keepalive on every connection every 2 s and records `CurrentRoundTripTime` per candidate pair (checked in `pion/ice` v4.4.9) | **Free** for pairs that already have a connection | Measures the real path, including TURN if used | **Use this** |
| Extra "probe" connections for members not in voice | One ICE connection per missing pair; keepalives are bytes per second | Same as above | Use it: at 10 people that's at most 45 pairs |
| Vivaldi network coordinates (each node gets coordinates whose distances predict RTT) | Few samples per node | Median error ≈ 11% on 1740 Internet hosts | Only needed beyond ~50 people, where a full mesh stops being cheap |
| RTT to the signaling server, added up | Free | Poor: two people in one city can both be far from the server | No |

The ICE data also tells us **how** each pair connects: `host`/`srflx`
(direct) or `relay` (through TURN). A tree edge over TURN doubles the TURN
server's traffic and adds latency, so TURN edges get a large penalty or are
not allowed as relay edges.

**Per-hop costs besides latency:** every extra hop adds a node that can
fail, adds its loss to the path (`P(path ok) = Π (1 − p_hop)`), and makes
keyframe requests travel further. That is why depth stays ≤ 3.

---

## Several supernodes per stream: a worked example

A room in two cities. One-way latency is 5 ms within a city and 20 ms
between them; forwarding adds `f = 2` ms. Bitrate 8 Mbps, so one copy =
10.6 Mbps.

| Node | City | Upload | Seeding | Copies |
|---|---|---|---|---|
| S (streamer) | X | — | budget 1 (or 2) | 1 (2) |
| A | Y | 100 Mbps, wired | on, no cap | 9 |
| B | X | 40 Mbps | on | 3 |
| C | X | 25 Mbps | on | 2 |
| D | X | 50 Mbps, Wi-Fi | on, cap 20 Mbps | 1 |
| E | X | — | off | 0 |
| F, G, H, I | Y | — | off | 0 |

**Option 1: today's rule, one biggest supernode** (A, wired with the most
upload). S → A → everyone.

| Watchers | Latency |
|---|---|
| A | 20 |
| F, G, H, I (near A) | 20 + 2 + 5 = **27** |
| B, C, D, E (near the streamer) | 20 + 2 + 20 = **42** |

Worst **42 ms**, mean **32.9 ms**. People sitting next to the streamer get
the worst latency: the video crosses the country twice.

**Option 2: latency-aware tree, streamer still sends one copy.**
S → B; B → A, C, D; C → E; A → F, G, H, I.

| Watchers | Latency |
|---|---|
| B | 5 |
| C, D | 5 + 2 + 5 = 12 |
| E | 12 + 2 + 5 = 19 |
| A | 5 + 2 + 20 = 27 |
| F, G, H, I | 27 + 2 + 5 = **34** |

Worst **34 ms** (−19%), mean **23.4 ms** (−29%), same streamer load. Three
relays instead of one, and no relay serves more than 4. That is close to the
best possible here: anyone in city Y needs at least one cross-city hop after
the streamer's single copy.

**Option 3: streamer allows two copies.** S → A and S → B; B → C, D, E;
A → F, G, H, I.

| Watchers | Latency |
|---|---|
| B | 5 |
| C, D, E | 12 |
| A | 20 |
| F, G, H, I | **27** |

Worst **27 ms**, mean **18.8 ms**, at the cost of 10.6 Mbps more of the
streamer's upload.

**What the example shows:**

- Ranking by "biggest upload" is the wrong rule once people are spread out.
- A tree spreads the load: the worst relay goes from 8 copies to 4, and a
  relay failure stalls 4 people instead of 8 (smaller blast radius).
- The streamer's second copy is a real trade-off with a price tag that can be
  shown to the user ("+10.6 Mbps of your upload saves up to 7 ms for half the
  room"). It should be the streamer's choice, not automatic.

---

## Protecting the streamer

The streamer is playing. Its machine and its connection are the most
latency-sensitive in the room.

1. **Budget, default 1 copy.** The streamer sends the stream to one relay
   and relays nothing else. It never hosts other streams (D8 already stops it
   watching).
2. **Even one copy is a lot of upload.** At 8 Mbps the copy needs 10.6 Mbps.
   On a 20 Mbps uplink that is over half the link, and a nearly full uplink
   **queues the game's own packets** (bufferbloat), raising the player's
   in-game ping. Rule: warn when the stream needs more than about 60% of the
   streamer's measured upload, and suggest a lower bitrate.
3. **Watch the streamer's queue, not just its rate.** If the RTT on the
   streamer's link to its relay rises well above its idle baseline while
   streaming, the uplink is queuing. That is a cheap, passive signal (no extra
   traffic) that the bitrate is too high for this connection.
4. **Allow an opt-in second copy** (option 3 above), only when the budget
   check passes, and show what it buys.
5. **Never fall back to "the streamer sends to everyone" silently.** Today's
   degraded mode can do that. Instead: degraded with a suggested bitrate
   (`r*`), and the streamer decides.

---

## Seeding

A member chooses whether, and how much, they relay for others.

**Modes:**

| Mode | Meaning |
|---|---|
| `off` | Never relay. Still watch normally |
| `on` | Relay up to `maxUploadMbps`, only for the stream I watch |
| `on` + `anyStream` | Also relay a stream I'm not watching ("pure seeder"): costs my download too, only used when no watcher can relay |

**Options** (all optional):

- `maxUploadMbps`: the cap. The app shows it as copies ("≈ 2 viewers at
  1080p60") and as data volume.
- `notWhileGaming` (default on): stop relaying while a game is in the
  foreground on this PC.
- `onlyOnPower` (default on for laptops): stop relaying on battery.

**The cap is enforced by tree shape.** The server never gives a node more
children than its copies. It does not throttle traffic: dropping packets of a
live video means visible corruption for every child. If a node's real send
rate exceeds its cap (keyframe bursts, a bitrate change), the server moves a
child elsewhere at the next decision.

**Data volume is part of consent.** One copy at 8.1 Mbps is about **3.7 GB
per hour**. A relay with 4 copies sends about 15 GB per hour, so 44 GB in a
three-hour session. On a metered or capped connection that matters, so the
app shows it and seeding is never switched on silently.

**Default:** `off`, with the app asking once ("Help your friends' streams?
Share up to … Mbps"), the same way it handles the mic mode: the app stores the
answer. The tension: with seeding off everywhere and a 1-copy streamer, a
stream can only serve one watcher at full quality. The server should say so
plainly ("this stream needs 2 more seeders, or a lower bitrate"), and the app
can nudge.

**Bring your own seeder.** The same code can run headless on a cloud VM
(`cast seed -room …`) as a member that seeds `anyStream` and never watches.
That is a personal relay server for the group. Hyperscalers charge per GB
(3.7 GB per copy-hour adds up), while some VPS plans include several TB of
traffic per month, which makes it cheap. Optional, never required.

---

## The check: what to measure and when

**Rule: the heartbeat never waits.** The 250 ms status heartbeat stays as it
is. Measurements run in their own goroutine and write their latest results
to a snapshot; the heartbeat reads the snapshot. It attaches a `metrics`
block only when a value changed beyond a threshold, or every 5 s at most.

**What to measure, and how often:**

| Signal | Source | Cost | When |
|---|---|---|---|
| RTT to every peer, path type | ICE stats on the mesh | Free | Continuously (every 2 s from keepalives) |
| Loss and jitter seen by each child | RTCP receiver reports, already flowing | Free | Continuously |
| What I'm actually sending | Byte counters per link | Free | Every 1 s |
| Queuing on my uplink | RTT rise above the idle baseline while sending; with Pion's send-side bandwidth estimator (`pion/interceptor/pkg/gcc`, not registered today), its estimate too | Free / small | Continuously while sending |
| CPU (system idle, own process) | OS counters | Microseconds | Every 2 s |
| Free memory | OS | Microseconds | Every 10 s (gate only) |
| Wired / Wi-Fi, interface change | OS | Microseconds | On change |
| On battery, thermal throttling | OS | Microseconds | On change |
| Gaming: a fullscreen app in front, or the GPU busy | OS, platform-specific | Microseconds | Every 5 s |
| **Upload capacity** | **Active probe** | ≈ 1–3 MB | On join; after an interface change; before a promotion beyond proven capacity; on request. **Never on a timer during a stream** |

**Why not run a speed test on every heartbeat, or periodically?**

- **It competes with what it measures.** A probe on a node that is relaying
  or gaming takes bandwidth from the live stream and the game, and it raises
  queuing delay for everything on that link.
- **Small tests measure the wrong thing.** A burst of a few packets measures
  link **capacity** (the dispersion of back-to-back packets), not **available
  bandwidth**, which is what we need. Trains of packets give a bound between
  the two. Wi-Fi frame aggregation makes small bursts look faster than they
  are.
- **Most of the information is already free.** A node that has been sending
  4 copies for a minute with no loss and no RTT rise has **proven** 4 copies.
  No test can say more than that.

**The precise probe: before promoting.** When the election wants node X to
send `k` copies and X has not proven `k`, X sends padding over its existing
idle links to the exact children it would serve, at the target rate, for
about 0.5 s. The children report received rate, loss and whether their
one-way delay was trending upward (the delay trend is the SLoPS idea behind
pathload: delay rises once you send faster than the available bandwidth).
Cost: `k × 10.6 Mbps × 0.5 s`, so about 2.7 MB for 4 copies. It measures the
real paths at the real rate.

**Estimate bookkeeping per node:**

- `proven`: the highest rate sustained for ≥ 30 s with loss < 1% and no
  queuing; it decays slowly (it is old news after a network change).
- `probed`: the last probe's result; trusted for 10 minutes.
- `U_i = max(proven, probed)`, falling back to the join probe.

**Trust, but verify.** Children's receiver reports show what a relay actually
delivers. A node whose delivery falls short of its claim is moved down,
whatever it reports. That also covers a member with a misconfigured or lying
client.

---

## Failover with many relays

Today: one host plus one warm backup with idle links to every viewer;
failover ≈ 570 ms on a hang, ≈ 20 ms on a leave (measured).

With several relays, every relay needs a fallback. Two designs:

**A. Re-solve on failure.** The server excludes the failed relay and runs the
election again. The new edges need new connections, which take a connection
setup plus a keyframe (about a second). Simple, but slower than today.

**B. One mesh, many trees (recommended).** Every pair of members keeps
**one** pre-negotiated connection carrying voice, plus one video and one
game-audio track in each direction, idle by default. This works because,
under D6 and D8, a node forwards **at most one stream**: its own or the one it
watches. So one outgoing video track per pair is always enough, whatever the
tree looks like.

- A tree change is `ReplaceTrack` on the right senders (idle → live), the
  mechanism the backup already uses, plus a keyframe request. No SDP, no ICE,
  no new connection.
- Failover becomes: detection + server decision + one message + one keyframe.
- The connections already exist for voice; non-voice members get the same
  connection without the voice track.
- Cost: up to 45 connections in a room of 10, idle except keepalives and
  RTCP. Memory per connection is small (measure it with `-stats` before
  committing to this).
- Risk: one connection per pair carries both voice and video. That doesn't
  break D9 (voice survives streams), because tree changes never renegotiate
  the connection. A congested pair affects both, but they share the network
  path anyway.

**Optional, later: local failover.** The topology names a **backup parent**
for each watcher. A watcher whose parent stalls for 400 ms asks its backup
parent directly over the mesh and tells the server afterwards. That would cut
failover to roughly the stall threshold, but backup parents must keep spare
copies reserved, which costs capacity. Measure design B first; today's 570 ms
may be good enough.

---

## Stability

Every tree change costs a keyframe (a visible bitrate spike) and possibly a
brief glitch, so the tree must not chase noise.

1. **Change for a reason.** Change the tree immediately on: a failure, a
   capacity violation (a relay is over its copies or is queuing), someone
   joining, leaving or switching. Otherwise change only to improve latency.
2. **Hysteresis.** An improvement-only change needs the worst latency to
   improve by at least `max(10 ms, 20%)`, and the current tree to be at least
   30 s old.
3. **Bucketing.** Latencies are rounded to 5 ms before comparing.
4. **Churn in the objective.** Among equal trees, keep the most parents
   unchanged.
5. **Prefer members who have been around.** Live-streaming studies found a
   small set of long-lived, stable peers carry most of a mesh's useful
   traffic, and mTreebone builds its backbone from them. Use time-in-session
   as a tie-breaker, and keep a recently failed relay out for 30 s (as the
   `suspects` list does today).

---

## The algorithm

```
elect(stream):
    nodes   = watchers + streamer
    copies  = per node, from the capacity formula (streamer: its budget)
    lat     = one-way latency matrix from mesh RTTs (+ relay penalties)

    if copies[streamer] >= |watchers|:
        candidate = star from the streamer                # rare with budget 1
    else:
        candidate = best_depth2(nodes)                    # exact search
        if not candidate.feasible:
            candidate = compact_tree_greedy(nodes, max_depth=3) then local swaps
    if not candidate.feasible:
        candidate = best effort tree
        candidate.degraded = true
        candidate.maxBitrate = r*                         # suggest to the streamer

    if must_change(current) or improves_enough(candidate, current):
        current = candidate
    plan probes for nodes asked to exceed their proven copies
    publish current (with a backup parent per watcher if local failover is on)

best_depth2(nodes):
    for each set R of up to copies[streamer] watchers (the streamer's children):
        binary-search T over sorted latency values:
            feasible(T) = max-flow assigns every other watcher to some k in R
                          with l(s,k) + f + p(k) + l(k,w) <= T and ≤ copies[k] each
        keep the R with the best (T, churn, mean, relays)
```

Run it on the signaling server (it has the global view; at this size the
work is trivial). Peers report; they don't choose their own parents, which
would oscillate as everyone chases the same good relay.

---

## More ideas

- **Fit the bitrate to the room.** Compute `r*` for the best tree and offer
  it to the streamer when full quality doesn't fit. Later, with the native
  encoder, apply it automatically within limits the streamer sets.
- **Temporal layers (future codec).** H.264 in WebRTC has no layered coding,
  so a relay forwards all or nothing. AV1 or VP9 with temporal layers would
  let a weak relay forward 30 fps of a 60 fps stream. Out of scope while
  H.264 is fixed.
- **Voice arrives before the video.** The streamer's voice goes directly
  over the mesh; the video takes the tree, plus encoding and decoding. Viewers
  may hear a reaction before they see its cause. The standard tolerance for
  sound leading picture is about 45 ms detectable and 90 ms acceptable
  (ITU-R BT.1359). Measure the real offset first; if it is over ~45 ms, delay
  only the **streamer's** voice on each watcher by the difference, capped.
  Conversation between viewers stays undelayed.
- **Privacy of metrics.** Metrics go only to the signaling server, coarsened
  (RTT in 5 ms buckets, CPU in 10% steps). Other members see the tree, not
  each other's machines.
- **Explain the tree.** Each `stream.updated` can carry a reason per relay
  ("B: nearest to the streamer, 3 copies free"), so the app can show who is
  helping and why.
- **Thank seeders.** Show in the UI who is relaying and how much. Socially,
  that is what keeps people opting in.

---

## Arguments on the proposal

| Proposal | Verdict | Why |
|---|---|---|
| Elect more than one supernode | **Agree** | A small tree beats a star on latency and blast radius, and it is cheap to optimise exactly at this size |
| Use latency between members | **Agree, and it's free** | The mesh already measures it; the example shows 19–36% lower worst latency |
| Don't make the streamer serve everyone | **Agree** | Default budget of 1 copy, opt-in second copy with its price shown; bufferbloat makes this matter even more than CPU |
| Run a check when the heartbeat arrives, without blocking it | **Agree on not blocking; disagree on the trigger** | Heartbeats are 4 per second; tie measurements to events instead, and keep the heartbeat as the carrier of cached results |
| Measure network speed in the check | **Disagree as a routine; agree as a targeted probe** | A routine speed test competes with the stream and the game, and small tests measure capacity, not what's available. Passive proof covers most cases; probe only before a promotion, on the exact links |
| Measure CPU and memory | **Agree, as gates** | They rarely limit forwarding; "is a game running" is the signal that matters |
| Measure disk | **Disagree** | Relays never touch disk |
| Promote on the basis of the check | **Agree, but as constraints, not a score** | Converting everything into copies and milliseconds avoids arbitrary weights between unlike units |
| A manual seeding flag | **Agree** | `off`/`on`, opt-in, asked once and remembered by the app |
| Explicit bandwidth to share | **Agree** | `maxUploadMbps`, shown as viewers and GB per hour, enforced by tree shape |

---

## What would change

**Spec ([AGENT_PROTOCOL.md](../cast/AGENT_PROTOCOL.md)):**

- `JoinParams.network.canHost` → `seeding: { mode: "off"|"on", maxUploadMbps?,
  anyStream?, notWhileGaming?, onlyOnPower? }`; new `network.setSeeding`.
- `StreamParams.maxCopies` (default 1): the streamer's budget.
- `Stream` gets `tree` (parent of each watcher, relay reasons) and
  `maxBitrateKbps` (`r*`) when degraded.
- Candidate decisions: **streamer budget 1 by default**, **seeding opt-in
  with a cap**, **measure passively, probe before promotion**.

**Code:**

| Change | Where |
|---|---|
| Metrics snapshot and its collectors (ICE RTTs, RTCP, counters, CPU, power, link type, gaming) | `peer`, `procstats` |
| `metrics` block in `status`, sent on change or every 5 s | `proto`, `peer/client.go` |
| Mesh connection for every pair, with idle video and game-audio tracks | `peer/voice.go` becomes the general mesh |
| Tree topology: parent per watcher; links derived from the parent map | `proto.Topology`, `peer/client.go` |
| Election: copies, latency matrix, exact depth-2 search, greedy depth 3, hysteresis | `signaling/election.go` |
| Failure detection per relay (watchers report their parent) | `signaling/server.go` |
| Pre-promotion probe (padding over idle links) and its report | `peer/links.go`, `proto` |
| Send-side bandwidth estimator (`pion/interceptor/pkg/gcc`) as a queuing signal | `peer/media.go` |

**Sequencing with [multi-stream](multi-stream.md):** multi-stream can land
first with one relay per stream and the streamer-last order. The **mesh
connection** (design B) should be decided before multi-stream Phase 4, since
both change how the client builds links.

---

## Open questions

1. **Seeding default:** `off` with a one-time prompt (proposed), or `on`
   with a small automatic cap such as one copy?
2. **Mesh for everyone:** accept up to 45 always-on connections in a room of
   10, to get free latency data and instant tree changes? Measured and
   brainstormed in [mesh.md](mesh.md): about 2.2 MB and 0.65% of a core per
   member; recommended, with a room cap and a "hide my IP" option.
3. **Pure seeders:** allow members to relay streams they don't watch, at the
   cost of their download?
4. **Streamer's second copy:** offered automatically when it saves more than
   some threshold, or only as a manual setting?
5. **Local failover:** worth the reserved capacity, or is server-driven
   failover fast enough once tree changes are track swaps?

---

## Sources

- Shi, Turner. *Multicast Routing and Bandwidth Dimensioning in Overlay
  Networks*, IEEE JSAC 2002 (minimum-diameter degree-limited trees, NP-hard,
  Compact Tree heuristic):
  [jsac02ss.pdf](https://arl.wustl.edu/~jon.turner/pubs/2002/jsac02ss.pdf);
  *Routing in Overlay Multicast Networks*, INFOCOM 2002:
  [infocom02ss.pdf](https://arl.wustl.edu/~jon.turner/pubs/2002/infocom02ss.pdf)
- Dabek, Cox, Kaashoek, Morris. *Vivaldi: A Decentralized Network Coordinate
  System*, SIGCOMM 2004 (median error ≈ 11% over 1740 hosts):
  [paper](https://vm-web.pdos.csail.mit.edu/papers/vivaldi:sigcomm/paper.pdf)
- Wang, Xiong, Liu. *mTreebone: A Hybrid Tree/Mesh Overlay for
  Application-Layer Live Video Multicast*, ICDCS 2007 / IEEE TPDS 2010
  (stable-node backbone):
  [mTreebone-icdcs2007.pdf](https://www2.cs.sfu.ca/~jcliu/Papers/mTreebone-icdcs2007.pdf)
- Dovrolis, Ramanathan, Moore. *Packet-Dispersion Techniques and a
  Capacity-Estimation Methodology*, IEEE/ACM ToN 2004 (packet pairs measure
  capacity; trains bound available bandwidth):
  [CAIDA](https://www-old.caida.org/publications/papers/2004/ton_dispersion)
- Jain, Dovrolis. *Pathload / SLoPS*, PAM 2002 and IEEE/ACM ToN 2003
  (one-way delay trend above the available bandwidth):
  [ACM](https://www.doi.org/10.1145/633025.633054),
  [ToN summary](https://typeset.io/pdf/end-to-end-available-bandwidth-measurement-methodology-4ouujj6c90.pdf)
- ITU-R BT.1359 lip-sync tolerances (≈ 45 ms lead detectable, 90 ms
  acceptable), as summarised in
  [Audio-to-video synchronization](https://en.wikipedia.org/wiki/Audio-to-video_synchronization)
  and [arXiv 2212.01686](https://arxiv.org/pdf/2212.01686)
- Pion source, checked locally: `pion/ice` v4.4.9 records
  `CurrentRoundTripTime` per candidate pair with a 2 s keepalive;
  `AvailableOutgoingBitrate` is not filled in; `pion/interceptor` v0.1.51
  ships `pkg/gcc` (send-side bandwidth estimation), which `cast` doesn't
  register yet.
- Measured in this repo: supernode CPU ≈ 16% of one core for 7 copies at
  8.7 Mbps; failover ≈ 570 ms (hang) and ≈ 20 ms (leave).
