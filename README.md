# sfx

| Project | What it is |
|---|---|
| [cast](cast/) | Low-latency H.264 1080p60 game streaming to a small group over WebRTC (Go). A viewer's PC acts as the forwarding "supernode", with sub-second failover to a warm backup, plus game audio and voice chat. See the [cast README](cast/README.md). |
| [voice](voice/) | The voice chat audio engine that `cast` runs as a sidecar (Rust): microphone, echo cancellation with sonora, Opus, mixing and playback. See the [voice README](voice/README.md). |
