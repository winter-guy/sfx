// Package proto defines the JSON messages exchanged over the signaling WebSocket.
package proto

const (
	RoleStreamer = "streamer"
	RoleViewer   = "viewer"
)

// Message types.
const (
	TypeJoin     = "join"     // client -> server, first message
	TypeWelcome  = "welcome"  // server -> client, carries the assigned peer ID
	TypeTopology = "topology" // server -> all, who streams / who hosts / who watches
	TypeSignal   = "signal"   // client <-> client (relayed), SDP offer/answer
	TypeError    = "error"    // server -> client, fatal
	TypeStatus   = "status"   // client -> server every 250ms: heartbeat + is my video flowing
)

// Status states for the link a peer plays (or, on the host, receives) from.
const (
	StateNone    = "none"    // not receiving anything (streamer, or no host)
	StateWaiting = "waiting" // link just became active, no packet yet
	StateFlowing = "flowing"
	StateStalled = "stalled" // packets stopped
)

// Status doubles as the application heartbeat.
type Status struct {
	Upstream string `json:"upstream"` // peer I receive video from
	State    string `json:"state"`
}

// Join announces a peer and what it can do.
type Join struct {
	Room        string  `json:"room"`
	Role        string  `json:"role"`
	Name        string  `json:"name"`
	UploadMbps  float64 `json:"uploadMbps"`
	Wired       bool    `json:"wired"`
	CanHost     bool    `json:"canHost"`               // viewer opted in to act as supernode
	BitrateKbps int     `json:"bitrateKbps,omitempty"` // streamer only: target video bitrate
	Voice       bool    `json:"voice,omitempty"`       // takes part in voice chat
}

// Topology is the server's decision about how media flows in a room.
//
// Epoch changes when the streamer or the host changes (informational).
// Version changes on every update and lets clients drop out-of-order messages.
// Clients derive the links they need from Streamer/Host/Backup/Viewers.
type Topology struct {
	Version     int               `json:"version"`
	Epoch       int               `json:"epoch"`
	Streamer    string            `json:"streamer"`
	Host        string            `json:"host"`
	Backup      string            `json:"backup"` // warm standby supernode (may be the streamer)
	Viewers     []string          `json:"viewers"`
	Voice       []string          `json:"voice,omitempty"` // peers in voice chat (full mesh)
	Names       map[string]string `json:"names"`
	BitrateKbps int               `json:"bitrateKbps"`
	Degraded    bool              `json:"degraded"` // no candidate had enough upload
	Reason      string            `json:"reason"`
}

// Signal carries a complete (non-trickle) SDP. Message.Link identifies the
// link instance (chosen by the sender) so stale answers are ignored.
type Signal struct {
	Kind string `json:"kind"` // "offer" | "answer"
	SDP  string `json:"sdp"`
}

// Message is the envelope for everything on the wire.
type Message struct {
	Type     string    `json:"type"`
	ID       string    `json:"id,omitempty"`
	From     string    `json:"from,omitempty"`
	To       string    `json:"to,omitempty"`
	Epoch    int       `json:"epoch,omitempty"`
	Link     string    `json:"link,omitempty"`
	Join     *Join     `json:"join,omitempty"`
	Topology *Topology `json:"topology,omitempty"`
	Signal   *Signal   `json:"signal,omitempty"`
	Status   *Status   `json:"status,omitempty"`
	Error    string    `json:"error,omitempty"`
}
