package peer

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// MeasureUploadMbps pushes data to the signaling server's /speedtest for
// roughly d and returns the rate the server observed. It measures the path to
// the signaling server, which is a proxy for the peer's real upload; a
// peer-to-peer probe can replace it later.
func MeasureUploadMbps(ctx context.Context, wsURL string, d time.Duration) (float64, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return 0, err
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	u.Path = "/speedtest"
	u.RawQuery = ""

	chunk := make([]byte, 64<<10)
	_, _ = rand.Read(chunk) // incompressible
	body := &timedReader{chunk: chunk, until: time.Now().Add(d)}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("speedtest: %s", resp.Status)
	}
	var out struct {
		Mbps float64 `json:"mbps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Mbps, nil
}

type timedReader struct {
	chunk []byte
	until time.Time
}

func (t *timedReader) Read(p []byte) (int, error) {
	if time.Now().After(t.until) {
		return 0, io.EOF
	}
	return copy(p, t.chunk), nil
}
