package client

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/chronologos/goet/internal/transport"
	"github.com/chronologos/goet/internal/version"
)

// logProfile emits a periodic RTT/loss line to stderr.
// Called from ioLoop on each heartbeat tick when Profile is enabled.
func (c *Client) logProfile(conn *transport.Conn) {
	stats := conn.QConn.ConnectionStats()
	fmt.Fprintf(c.stderr, "[profile] rtt=%s (min=%s smooth=%s jitter=%s) loss=%d/%dpkts\n",
		formatDuration(stats.LatestRTT),
		formatDuration(stats.MinRTT),
		formatDuration(stats.SmoothedRTT),
		formatDuration(stats.MeanDeviation),
		stats.PacketsLost,
		stats.PacketsSent,
	)
}

// logProfileSummary emits a final summary to stderr and writes JSON to /tmp.
// Called via defer in ioLoop when Profile is enabled.
func (c *Client) logProfileSummary(conn *transport.Conn) {
	stats := conn.QConn.ConnectionStats()
	duration := time.Since(c.profileStart)
	fmt.Fprintf(c.stderr, "[profile] === Connection Profile ===\n")
	fmt.Fprintf(c.stderr, "[profile] Duration: %s\n", duration.Round(time.Second))
	fmt.Fprintf(c.stderr, "[profile] RTT: min=%s smooth=%s latest=%s jitter=%s\n",
		formatDuration(stats.MinRTT),
		formatDuration(stats.SmoothedRTT),
		formatDuration(stats.LatestRTT),
		formatDuration(stats.MeanDeviation),
	)
	fmt.Fprintf(c.stderr, "[profile] Traffic: sent=%s/%dpkts recv=%s/%dpkts lost=%dpkts\n",
		formatBytes(stats.BytesSent),
		stats.PacketsSent,
		formatBytes(stats.BytesReceived),
		stats.PacketsReceived,
		stats.PacketsLost,
	)

	c.writeProfileJSON(conn)
}

// profileJSON is the structured output written to /tmp.
type profileJSON struct {
	Timestamp string         `json:"timestamp"`
	Commit    string         `json:"commit"`
	Host      string         `json:"host"`
	DurationS float64        `json:"duration_s"`
	RTT       profileRTT     `json:"rtt"`
	Traffic   profileTraffic `json:"traffic"`
}

type profileRTT struct {
	MinMs    float64 `json:"min_ms"`
	SmoothMs float64 `json:"smooth_ms"`
	LatestMs float64 `json:"latest_ms"`
	JitterMs float64 `json:"jitter_ms"`
}

type profileTraffic struct {
	BytesSent uint64 `json:"bytes_sent"`
	BytesRecv uint64 `json:"bytes_recv"`
	PktsSent  uint64 `json:"pkts_sent"`
	PktsRecv  uint64 `json:"pkts_recv"`
	PktsLost  uint64 `json:"pkts_lost"`
}

// writeProfileJSON dumps a JSON profile to /tmp/goet-profile-<timestamp>.json.
func (c *Client) writeProfileJSON(conn *transport.Conn) {
	stats := conn.QConn.ConnectionStats()
	now := time.Now()
	duration := now.Sub(c.profileStart)

	p := profileJSON{
		Timestamp: now.UTC().Format(time.RFC3339),
		Commit:    version.Commit,
		Host:      c.cfg.Host,
		DurationS: duration.Seconds(),
		RTT: profileRTT{
			MinMs:    msFloat(stats.MinRTT),
			SmoothMs: msFloat(stats.SmoothedRTT),
			LatestMs: msFloat(stats.LatestRTT),
			JitterMs: msFloat(stats.MeanDeviation),
		},
		Traffic: profileTraffic{
			BytesSent: stats.BytesSent,
			BytesRecv: stats.BytesReceived,
			PktsSent:  stats.PacketsSent,
			PktsRecv:  stats.PacketsReceived,
			PktsLost:  stats.PacketsLost,
		},
	}

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		log.Printf("profile: json marshal: %v", err)
		return
	}

	filename := fmt.Sprintf("/tmp/goet-profile-%s.json", now.Format("20060102-150405"))
	if err := os.WriteFile(filename, data, 0644); err != nil {
		log.Printf("profile: write %s: %v", filename, err)
		return
	}

	fmt.Fprintf(c.stderr, "[profile] wrote %s\n", filename)
}

// msFloat converts a Duration to milliseconds as float64.
func msFloat(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// formatBytes formats a byte count as a human-readable string.
func formatBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// formatDuration formats a duration as milliseconds with one decimal.
func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0ms"
	}
	ms := float64(d) / float64(time.Millisecond)
	return fmt.Sprintf("%.1fms", ms)
}
