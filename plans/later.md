# Later

Ideas worth doing, parked until the core work is done. Each one links to
where it was discussed. Move an idea into a plan, or into the decisions in
[cast/AGENT_PROTOCOL.md](../cast/AGENT_PROTOCOL.md#decisions), when it's
picked up.

## Streaming and relays

| Idea | What it is | Why later | From |
|---|---|---|---|
| **Bring your own seeder** | Run `cast seed -room …` headless on a cloud VM: a member that relays any stream and never watches. A personal relay server for the group | Needs seeding (and pure seeders) first; hosting costs vary a lot by provider (per-GB egress vs. included traffic) | [supernode-election.md § Seeding](supernode-election.md#seeding) |
| **Explain the tree** | `stream.updated` carries a reason per relay ("B: nearest to the streamer, 3 copies free") so the app can show who relays and why | Needs the tree election | [§ More ideas](supernode-election.md#more-ideas) |
| **Thank seeders** | The UI shows who is relaying and how much | Needs seeding and the reasons above | [§ More ideas](supernode-election.md#more-ideas) |
| **Verify relays** | Compare what a relay claims with what its children's receiver reports show it delivers; move under-delivering relays down, whatever they report | Needs the metrics in `status` | [§ The check](supernode-election.md#the-check-what-to-measure-and-when) |
| **Fit the bitrate to the room** | Compute the highest bitrate the best tree can carry (`r*`) and offer it to the streamer; later, apply it automatically with the native encoder | Needs the tree election; automatic only with the native encoder | [§ The problem as math](supernode-election.md#the-problem-as-math) |
| **Temporal layers** | With AV1 or VP9 temporal layers, a weak relay could forward 30 fps of a 60 fps stream | H.264 is fixed for now | [§ More ideas](supernode-election.md#more-ideas) |
| **Local failover** | Each watcher has a backup parent and switches to it itself after a 400 ms stall | Costs reserved capacity; measure server-driven failover with track swaps first | [§ Failover with many relays](supernode-election.md#failover-with-many-relays) |
| **Instant stream switching** | Keep idle links to other streams warm so switching back is nearly instant | Mostly solved by the mesh with idle tracks, if adopted | [AGENT_PROTOCOL.md, open question 3](../cast/AGENT_PROTOCOL.md#open-questions) |
| **Watching while streaming** | Let a streamer watch another stream; the sidecar needs a second game-audio input | Excluded by D8 for v1 | [AGENT_PROTOCOL.md, open question 2](../cast/AGENT_PROTOCOL.md#open-questions) |

## Voice

| Idea | What it is | Why later | From |
|---|---|---|---|
| **Voice and video offset** | The streamer's voice goes directly over the mesh while the video takes the tree, so viewers may hear a reaction before seeing it. Measure the offset; if it's over ~45 ms (detectable per ITU-R BT.1359), delay only the streamer's voice on each watcher by the difference, capped | Measure first; may not be noticeable | [§ More ideas](supernode-election.md#more-ideas) |

## Privacy

| Idea | What it is | Why later | From |
|---|---|---|---|
| **Coarse metrics** | Send metrics only to the signaling server, rounded (RTT in 5 ms steps, CPU in 10% steps); members see the tree, not each other's machines | Needs the metrics in `status` | [§ More ideas](supernode-election.md#more-ideas) |
