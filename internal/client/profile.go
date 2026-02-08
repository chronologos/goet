package client

import (
	"fmt"
	"time"

	"github.com/chronologos/goet/internal/transport"
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

// logProfileSummary emits a final summary to stderr.
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
