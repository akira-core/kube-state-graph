package promql

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/internal/testlog"
)

func TestTruncateUTF8(t *testing.T) {
	assert.Equal(t, "abc", truncateUTF8("abc", 10))
	assert.Equal(t, "ab", truncateUTF8("abc", 2))
	// "é" is two bytes; a cut inside it backs off to the character start.
	assert.Equal(t, "a", truncateUTF8("aé", 2))
	assert.Equal(t, "aé", truncateUTF8("aé", 3))
	assert.Empty(t, truncateUTF8("é", 1))
}

// captureJSONLogs installs a Debug-level JSON slog default for one test and
// returns a function decoding every record written so far.
func captureJSONLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	buf := &testlog.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		var out []map[string]any
		sc := bufio.NewScanner(strings.NewReader(buf.String()))
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			var rec map[string]any
			require.NoError(t, json.Unmarshal(sc.Bytes(), &rec))
			out = append(out, rec)
		}
		return out
	}
}

// An over-long statement is truncated to maxStatementBytes on a character
// boundary in the span and in both query logs, which also carry the full
// length and a SHA-256 prefix of the full statement.
func TestClient_LongStatementBoundedInSpanAndLogs(t *testing.T) {
	exp := installTestTracer(t)
	records := captureJSONLogs(t)

	// 2047 ASCII bytes then two-byte characters: byte 2048 falls inside one.
	query := strings.Repeat("a", maxStatementBytes-1) + strings.Repeat("é", 10_000)
	sum := sha256.Sum256([]byte(query))
	wantSHA := hex.EncodeToString(sum[:])[:12]
	wantText := strings.Repeat("a", maxStatementBytes-1)

	c := &Client{api: fakeAPI{err: assert.AnError}}
	_, err := c.Instant(t.Context(), string(QPodInfo), query, time.Unix(1000, 0))
	require.Error(t, err)

	s := findQuerySpan(t, exp)
	stmt, ok := spanAttr(t, s, "db.statement")
	require.True(t, ok)
	assert.Equal(t, wantText, stmt)
	assert.True(t, utf8.ValidString(stmt))
	n, ok := spanInt(s, "kube_state_graph.query_bytes")
	require.True(t, ok)
	assert.EqualValues(t, len(query), n)
	sha, ok := spanAttr(t, s, "kube_state_graph.query_sha256")
	require.True(t, ok)
	assert.Equal(t, wantSHA, sha)

	var seen []string
	for _, rec := range records() {
		msg, _ := rec["msg"].(string)
		if msg != "promql query" && msg != "promql query failed" {
			continue
		}
		seen = append(seen, msg)
		assert.Equal(t, wantText, rec["query"], msg)
		assert.InDelta(t, float64(len(query)), rec["query_bytes"], 0, msg)
		assert.Equal(t, wantSHA, rec["query_sha256"], msg)
	}
	assert.ElementsMatch(t, []string{"promql query", "promql query failed"}, seen)
}

// A short statement is carried whole; the length and digest are still set.
func TestClient_ShortStatementCarriedWhole(t *testing.T) {
	exp := installTestTracer(t)
	c := &Client{api: fakeAPI{val: model.Vector{}}}
	_, err := c.Instant(t.Context(), string(QPodInfo), "kube_pod_info", time.Unix(1000, 0))
	require.NoError(t, err)

	s := findQuerySpan(t, exp)
	stmt, _ := spanAttr(t, s, "db.statement")
	assert.Equal(t, "kube_pod_info", stmt)
	n, _ := spanInt(s, "kube_state_graph.query_bytes")
	assert.EqualValues(t, len("kube_pod_info"), n)
	sha, _ := spanAttr(t, s, "kube_state_graph.query_sha256")
	assert.Len(t, sha, 12)
}
