package build

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureDebugRecords installs a Debug-level JSON slog handler for the duration
// of fn and returns the emitted records parsed as maps. The shared
// testlog.Capture helper is Info-level text, so the connection-string fallback
// debug lines (this feature) need their own Debug handler here.
func captureDebugRecords(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	fn()

	var recs []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		recs = append(recs, m)
	}
	return recs
}

func reasonSet(recs []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, r := range recs {
		if v, ok := r["reason"].(string); ok {
			out[v] = true
		}
	}
	return out
}

func hasMsg(recs []map[string]any, msg string) bool {
	for _, r := range recs {
		if r["msg"] == msg {
			return true
		}
	}
	return false
}

// Each scenario uses familyTopology(): nats.messaging resolves in prod-1, and
// the client "checkout"/abc is a real prod-1 pod, so the anchor is prod-1.
func TestParseServiceGraph_DebugLogsFallbackReasons(t *testing.T) {
	run := func(client, server, cluster, clientUID, serverUID string) []map[string]any {
		return captureDebugRecords(t, func() {
			parseServiceGraph(sampleVec(famSample(client, server, cluster, clientUID, serverUID)), familyTopology())
		})
	}

	t.Run("shadowed_by_uid", func(t *testing.T) {
		recs := run("checkout", "nats://nats.messaging.svc:4222", "prod-1", "abc", "zzz")
		assert.True(t, hasMsg(recs, "service-graph :// label SHADOWED by populated UID (resolved as pod, not service)"),
			"want the shadow debug line; got %v", recs)
		assert.True(t, reasonSet(recs)["conn_string_shadowed_by_uid"])
	})

	t.Run("host_unparseable_jdbc", func(t *testing.T) {
		recs := run("checkout", "jdbc:postgresql://nats.messaging.svc:5432/db", "prod-1", "abc", "")
		assert.True(t, reasonSet(recs)["conn_host_unparseable"], "got %v", recs)
	})

	t.Run("host_not_k8s_dns_single_label", func(t *testing.T) {
		recs := run("checkout", "redis://nats:6379", "prod-1", "abc", "")
		assert.True(t, reasonSet(recs)["conn_host_not_k8s_dns"], "got %v", recs)
	})

	t.Run("anchor_cluster_lacks_service", func(t *testing.T) {
		// data/queue exists in no cluster of familyTopology.
		recs := run("checkout", "amqp://queue.data.svc:5672", "prod-1", "abc", "")
		assert.True(t, reasonSet(recs)["anchor_cluster_lacks_service"], "got %v", recs)
	})

	t.Run("missing_uid_nonurl_label_D27", func(t *testing.T) {
		recs := run("checkout", "legacy-workload", "prod-1", "abc", "")
		assert.True(t, reasonSet(recs)["missing_uid_nonurl_label"], "got %v", recs)
	})

	t.Run("series_dropped_one_side_empty", func(t *testing.T) {
		recs := run("checkout", "", "prod-1", "abc", "")
		assert.True(t, hasMsg(recs, "service-graph series dropped (one side wholly empty: no UID and no label)"),
			"got %v", recs)
	})

	t.Run("resolved_service_emits_debug", func(t *testing.T) {
		recs := run("checkout", "nats://nats.messaging.svc:4222", "prod-1", "abc", "")
		assert.True(t, hasMsg(recs, "service-graph :// resolved to service node"), "got %v", recs)
	})

	t.Run("aggregated_summary_on_fallback", func(t *testing.T) {
		recs := run("checkout", "amqp://queue.data.svc:5672", "prod-1", "abc", "")
		assert.True(t, hasMsg(recs, "service-graph resolution fallbacks"), "got %v", recs)
	})
}
