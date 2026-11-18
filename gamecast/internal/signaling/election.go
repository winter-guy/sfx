package signaling

import (
	"fmt"
	"sort"
)

// HeadroomFactor is how much spare upload a host must have above the raw
// stream bitrate (packet overhead, retransmissions, action-scene peaks).
const HeadroomFactor = 1.3

// DefaultBitrateKbps is H.264 1080p60.
const DefaultBitrateKbps = 8000

// Candidate is a peer that could forward the stream.
type Candidate struct {
	ID         string
	IsStreamer bool
	UploadMbps float64
	Wired      bool
	CanHost    bool // viewers must opt in; the streamer always can
	JoinSeq    int
}

// Decision is the outcome of an election.
type Decision struct {
	Host     string
	Degraded bool
	Reason   string
}

// RequiredUploadMbps is the upload a host needs to serve `viewers` viewers.
// A host that is itself a viewer already has its copy, so it sends one fewer.
func RequiredUploadMbps(bitrateKbps, viewers int, hostIsStreamer bool) float64 {
	out := viewers
	if !hostIsStreamer {
		out--
	}
	if out < 0 {
		out = 0
	}
	return HeadroomFactor * float64(bitrateKbps) / 1000 * float64(out)
}

// Elect picks the supernode.
//
// Order of preference:
//  0. With one viewer the streamer sends directly (a relay saves nothing).
//  1. Keep the current host while it still has enough upload (avoids churn).
//  2. The streamer, if it can serve everyone directly (one hop less latency).
//  3. The best opted-in viewer that qualifies: wired first, then most upload,
//     then whoever joined earliest (more likely to be stable).
//  4. Nobody qualifies: degraded mode, pick whoever has the most spare upload.
//
// Viewers in exclude (the current host when picking a backup, or a host that
// was just declared dead) can't be picked but still count as viewers, so a
// backup is sized for the case where the failed host stays in the room.
func Elect(current string, streamer *Candidate, viewers []*Candidate, bitrateKbps int, exclude map[string]bool) Decision {
	if streamer == nil {
		return Decision{Reason: "no streamer"}
	}
	if bitrateKbps <= 0 {
		bitrateKbps = DefaultBitrateKbps
	}
	n := len(viewers)
	if n == 0 {
		return Decision{Host: streamer.ID, Reason: "no viewers"}
	}
	if n == 1 {
		// The streamer uploads one copy whatever we pick; a relay only adds a hop.
		return Decision{Host: streamer.ID, Reason: "single viewer: direct"}
	}

	need := func(c *Candidate) float64 { return RequiredUploadMbps(bitrateKbps, n, c.IsStreamer) }
	qualifies := func(c *Candidate) bool { return c.UploadMbps >= need(c) }

	eligible := []*Candidate{streamer}
	for _, v := range viewers {
		if v.CanHost && !exclude[v.ID] {
			eligible = append(eligible, v)
		}
	}

	for _, c := range eligible {
		if c.ID == current && qualifies(c) {
			return Decision{Host: c.ID, Reason: fmt.Sprintf("keeping current host (%.0f/%.0f Mbps)", c.UploadMbps, need(c))}
		}
	}

	if qualifies(streamer) {
		return Decision{Host: streamer.ID, Reason: fmt.Sprintf("streamer serves directly (%.0f/%.0f Mbps)", streamer.UploadMbps, need(streamer))}
	}

	var qualified []*Candidate
	for _, c := range eligible[1:] {
		if qualifies(c) {
			qualified = append(qualified, c)
		}
	}
	if len(qualified) > 0 {
		sort.SliceStable(qualified, func(i, j int) bool {
			a, b := qualified[i], qualified[j]
			if a.Wired != b.Wired {
				return a.Wired
			}
			if a.UploadMbps != b.UploadMbps {
				return a.UploadMbps > b.UploadMbps
			}
			return a.JoinSeq < b.JoinSeq
		})
		h := qualified[0]
		return Decision{Host: h.ID, Reason: fmt.Sprintf("supernode %s (%.0f/%.0f Mbps)", h.ID, h.UploadMbps, need(h))}
	}

	best := eligible[0]
	for _, c := range eligible[1:] {
		if c.UploadMbps-need(c) > best.UploadMbps-need(best) {
			best = c
		}
	}
	return Decision{
		Host:     best.ID,
		Degraded: true,
		Reason:   fmt.Sprintf("no candidate has enough upload; best is %s (%.0f/%.0f Mbps)", best.ID, best.UploadMbps, need(best)),
	}
}

// Pair is the elected supernode plus a warm backup.
type Pair struct {
	Decision
	// Backup keeps idle, pre-connected links so failover is a switch, not a
	// reconnect. It is the streamer when no viewer can take over (the
	// streamer then falls back to sending to everyone directly). Empty when
	// the streamer is the host: if the streamer is gone there is no stream.
	Backup string
}

// ElectPair picks the host and the backup. suspects are hosts recently
// declared dead; they stay in the room as viewers but can't be picked.
func ElectPair(curHost, curBackup string, streamer *Candidate, viewers []*Candidate, bitrateKbps int, suspects map[string]bool) Pair {
	if streamer == nil {
		return Pair{Decision: Decision{Reason: "no streamer"}}
	}
	present := map[string]bool{streamer.ID: true}
	for _, v := range viewers {
		present[v.ID] = true
	}
	// Host gone or dead: prefer the warm backup, it is already connected.
	if !present[curHost] || suspects[curHost] {
		curHost = curBackup
	}
	host := Elect(curHost, streamer, viewers, bitrateKbps, suspects)
	p := Pair{Decision: host}
	if host.Host == "" || host.Host == streamer.ID {
		return p
	}
	exclude := map[string]bool{host.Host: true}
	for id := range suspects {
		exclude[id] = true
	}
	if curBackup == host.Host {
		curBackup = ""
	}
	p.Backup = Elect(curBackup, streamer, viewers, bitrateKbps, exclude).Host
	return p
}
