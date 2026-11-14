package build

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akira-core/kube-state-graph/pkg/graph"
)

// The secret every scenario below plants in a connection string's userinfo.
// Nothing derived from a service-graph series — node id, node name, Debug
// attribute — may ever carry it.
const userinfoSecret = "s3cr3t-pw"

func TestStripConnStringUserinfo(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"user and password", "mongodb://app:" + userinfoSecret + "@mongo.example:27017/db", "mongodb://mongo.example:27017/db"},
		{"username only", "mongodb+srv://admin@cluster0.example/db", "mongodb+srv://cluster0.example/db"},
		{"empty username", "redis://:" + userinfoSecret + "@cache.example:6379", "redis://cache.example:6379"},
		{"jdbc double scheme url.Parse cannot split", "jdbc:postgresql://app:" + userinfoSecret + "@db.example:5432/orders", "jdbc:postgresql://db.example:5432/orders"},
		{"multi-host authority", "mongodb://app:" + userinfoSecret + "@h1:27017,h2:27017/db?replicaSet=rs0", "mongodb://h1:27017,h2:27017/db?replicaSet=rs0"},
		{"unescaped @ in password", "redis://:p@" + userinfoSecret + "@cache.example:6379", "redis://cache.example:6379"},
		{"password url.Parse rejects", "postgres://app:%zz" + userinfoSecret + "@db.example/x", "postgres://db.example/x"},
		{"authority ends at query", "amqp://app:" + userinfoSecret + "@queue.data.svc?heartbeat=10", "amqp://queue.data.svc?heartbeat=10"},
		{"authority ends at fragment", "amqp://app:" + userinfoSecret + "@queue.data.svc#x", "amqp://queue.data.svc#x"},
		{"every scheme-delimited authority", "jdbc:x://a:" + userinfoSecret + "@h/db?fallback=y://b:" + userinfoSecret + "@h2", "jdbc:x://h/db?fallback=y://h2"},
		{"no userinfo", "nats://nats.messaging.svc:4222", "nats://nats.messaging.svc:4222"},
		{"@ in path is not userinfo", "https://api.example/users/@me", "https://api.example/users/@me"},
		{"@ in query is not userinfo", "https://api.example/x?email=a@b.example", "https://api.example/x?email=a@b.example"},
		{"@ in fragment is not userinfo", "https://api.example#a@b", "https://api.example#a@b"},
		{"not a connection string", "user:pw@legacy-host", "user:pw@legacy-host"},
		{"empty", "", ""},
		{"bare separator", "://", "://"},
		{"userinfo only", "x://u:" + userinfoSecret + "@", "x://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripConnStringUserinfo(tc.in))
		})
	}
}

// A label that carries no userinfo is returned as-is, without an allocation:
// the helper runs on every series' labels, credentials or not.
func TestStripConnStringUserinfo_NoAllocWithoutUserinfo(t *testing.T) {
	for _, in := range []string{"nats://nats.messaging.svc:4222", "legacy-workload", "https://api.example/users/@me"} {
		allocs := testing.AllocsPerRun(100, func() { _ = stripConnStringUserinfo(in) })
		assert.Zero(t, allocs, in)
	}
}

// Every external fallback of connection-string resolution drops the userinfo
// from the node's id and name, while resolution itself still reads the raw
// label (so a credentialled label that DOES name a held Service still resolves).
func TestParseServiceGraph_ConnStringExternalStripsUserinfo(t *testing.T) {
	cases := []struct {
		name, server, wantExt string
	}{
		{"anchor lacks service", "amqp://app:" + userinfoSecret + "@queue.data.svc:5672", "amqp://queue.data.svc:5672"},
		{"host not k8s dns", "redis://app:" + userinfoSecret + "@nats:6379", "redis://nats:6379"},
		{"host unparseable (jdbc)", "jdbc:postgresql://app:" + userinfoSecret + "@nats.messaging.svc:5432/db", "jdbc:postgresql://nats.messaging.svc:5432/db"},
		{"password url.Parse rejects", "postgres://app:%zz@nats.messaging.svc/db", "postgres://nats.messaging.svc/db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := parseServiceGraph(sampleVec(famSample("checkout", tc.server, "prod-1", "abc", "")), familyTopology())
			require.Len(t, res.ExternalNodes, 1)
			ext := res.ExternalNodes[0]
			assert.Equal(t, graph.ExternalID(tc.wantExt), ext.IDValue)
			assert.Equal(t, tc.wantExt, ext.NameValue)
			assert.Empty(t, ext.LabelsValue)
			require.Len(t, res.Edges, 1)
			assert.Equal(t, graph.ExternalID(tc.wantExt), res.Edges[0].Target)
		})
	}

	t.Run("client side", func(t *testing.T) {
		res := parseServiceGraph(sampleVec(famSample("amqp://app:"+userinfoSecret+"@queue.data.svc:5672", "nats-0", "prod-1", "", "n1")), familyTopology())
		require.Len(t, res.ExternalNodes, 1)
		assert.Equal(t, graph.ExternalID("amqp://queue.data.svc:5672"), res.ExternalNodes[0].IDValue)
		require.Len(t, res.Edges, 1)
		assert.Equal(t, graph.ExternalID("amqp://queue.data.svc:5672"), res.Edges[0].Source)
	})

	t.Run("credentialled label naming a held service still resolves", func(t *testing.T) {
		res := parseServiceGraph(sampleVec(famSample("checkout", "nats://app:"+userinfoSecret+"@nats.messaging.svc:4222", "prod-1", "abc", "")), familyTopology())
		assert.Empty(t, res.ExternalNodes)
		assert.Equal(t, []string{"prod-1/messaging/nats"}, svcNodeIDs(res))
	})

	t.Run("filtered build out-of-scope service", func(t *testing.T) {
		res := parseServiceGraphScoped(sampleVec(famSample("checkout", "amqp://app:"+userinfoSecret+"@queue.data.svc:5672", "prod-1", "abc", "")), familyTopology(), nil, true)
		require.Len(t, res.ExternalNodes, 1)
		assert.Equal(t, graph.ExternalID("amqp://queue.data.svc:5672"), res.ExternalNodes[0].IDValue)
	})
}

// Labels that differ only in their credentials name the same endpoint, so they
// collapse onto ONE external node and ONE edge — the id is stable across a
// credential rotation inside the window.
func TestParseServiceGraph_ConnStringCredentialsCollapse(t *testing.T) {
	a := famSample("checkout", "amqp://app:old-pw@queue.data.svc:5672", "prod-1", "abc", "")
	b := famSample("checkout", "amqp://app:new-pw@queue.data.svc:5672", "prod-1", "abc", "")
	b.Metric["connection_type"] = "rotated" // distinct series identity
	res := parseServiceGraph(sampleVec(a, b), familyTopology())
	require.Len(t, res.ExternalNodes, 1)
	assert.Equal(t, "amqp://queue.data.svc:5672", res.ExternalNodes[0].NameValue)
	require.Len(t, res.Edges, 1)
}

// No Debug record the parse emits may carry the userinfo of a connection
// string, whichever per-series path the series takes. The assertion scans the
// whole captured output, not one message, so a new Debug site that logs a raw
// label fails here.
func TestParseServiceGraph_DebugLogsNeverCarryUserinfo(t *testing.T) {
	secretLabel := "amqp://app:" + userinfoSecret + "@queue.data.svc:5672"
	scenarios := map[string]model.Sample{
		"external fallback (anchor lacks service)": famSample("checkout", secretLabel, "prod-1", "abc", ""),
		"external fallback (not k8s dns)":          famSample("checkout", "redis://app:"+userinfoSecret+"@nats:6379", "prod-1", "abc", ""),
		"external fallback (jdbc)":                 famSample("checkout", "jdbc:postgresql://app:"+userinfoSecret+"@nats.messaging.svc:5432/db", "prod-1", "abc", ""),
		"resolved to service":                      famSample("checkout", "nats://app:"+userinfoSecret+"@nats.messaging.svc:4222", "prod-1", "abc", ""),
		"shadowed by populated UID (server)":       famSample("checkout", "nats://app:"+userinfoSecret+"@nats.messaging.svc:4222", "prod-1", "abc", "n1"),
		"shadowed by populated UID (client)":       famSample(secretLabel, "nats-0", "prod-1", "abc", "n1"),
		"client-side external":                     famSample(secretLabel, "nats-0", "prod-1", "", "n1"),
		"one side wholly empty":                    famSample(secretLabel, "", "prod-1", "", ""),
		"peer ladder with credentialled client":    famSample(secretLabel, "unknown", "prod-1", "abc", ""),
	}
	for name, s := range scenarios {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			parseServiceGraph(sampleVec(s), familyTopology())

			out := buf.String()
			require.NotEmpty(t, out, "the scenario must reach at least one Debug site")
			assert.NotContains(t, out, userinfoSecret)
			assert.False(t, strings.Contains(out, "app:"), "no username either: %s", out)
		})
	}
}
