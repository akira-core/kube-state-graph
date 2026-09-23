package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	promqlmocks "github.com/akira-core/kube-state-graph/pkg/promql/mocks"
)

// upstreamDialErr mimics the wrapped promql client error produced when the
// internal VictoriaMetrics endpoint is unreachable. It deliberately embeds the
// internal URL, hostname, and IP that must never reach a response body.
func upstreamDialErr() error {
	return errors.New(`Post "http://vm.internal:8428/api/v1/query": dial tcp 10.0.3.4:8428: connect: connection refused`)
}

// assertNoUpstreamLeak asserts the human message carries none of the internal
// upstream coordinates embedded in upstreamDialErr.
func assertNoUpstreamLeak(t *testing.T, message string) {
	t.Helper()
	assert.NotContains(t, message, "http://", "internal upstream URL must not leak")
	assert.NotContains(t, message, "dial tcp", "dial error detail must not leak")
	assert.NotContains(t, message, "vm.internal", "internal upstream host must not leak")
	assert.NotContains(t, message, "10.0.3.4", "internal upstream IP must not leak")
	assert.NotContains(t, message, "8428", "internal upstream port must not leak")
}

// TestGraphEndpoint_Upstream502_SanitisedMessage asserts the 502 envelope for a
// failed /v1/graph build keeps the contractual reason "upstream" but replaces
// the raw error (which embeds the internal VictoriaMetrics URL/host/IP) with a
// static message.
func TestGraphEndpoint_Upstream502_SanitisedMessage(t *testing.T) {
	s := newServerWithMocks(t, newErrQuerier(t, upstreamDialErr()), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)

	var body errReason
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "upstream", body.Error.Reason, "reason string is a contract and must not change")
	// Every query fails, so which family's error the build reports first is a
	// race; the message names ONE family and nothing from the upstream text.
	assert.True(t, strings.HasPrefix(body.Error.Message, "upstream query failed: "), body.Error.Message)
	assertNoUpstreamLeak(t, body.Error.Message)
}

// nameFailQuerier fails every query issued under the family name failing and
// answers every other query with an empty vector.
func nameFailQuerier(t *testing.T, failing string, err error) *promqlmocks.MockQuerier {
	t.Helper()
	q := promqlmocks.NewMockQuerier(t)
	q.EXPECT().Instant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name, _ string, _ time.Time) (model.Vector, error) {
			if name == failing {
				return nil, err
			}
			return model.Vector{}, nil
		}).
		Maybe()
	return q
}

// Spec (storage-graph-api "Storage build fails closed on upstream query
// errors"): a family /v1/graph degrades fails the storage request with a 502
// naming the family and nothing of the upstream error text; ALERTS does not.
func TestStorageGraphEndpoint_FailsClosedNamingFamily(t *testing.T) {
	const path = "/v1/storage-graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z&az=zone-a&env=prod"
	for _, family := range []string{"volume_labels", "aggr_space_used", "kubelet_volume_stats_used_bytes"} {
		t.Run(family, func(t *testing.T) {
			s := newServerWithMocks(t, nameFailQuerier(t, family, upstreamDialErr()), nil)
			srv := httptest.NewServer(s.Handler())
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + path)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusBadGateway, resp.StatusCode)

			var body errReason
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
			assert.Equal(t, "upstream", body.Error.Reason)
			assert.Equal(t, "upstream query failed: "+family, body.Error.Message)
			assertNoUpstreamLeak(t, body.Error.Message)
		})
	}

	t.Run("ALERTS", func(t *testing.T) {
		s := newServerWithMocks(t, nameFailQuerier(t, "ALERTS", upstreamDialErr()), nil)
		srv := httptest.NewServer(s.Handler())
		t.Cleanup(srv.Close)

		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

// Spec: "The graph endpoint keeps degrading".
func TestGraphEndpoint_OptionalFamilyStillDegrades(t *testing.T) {
	s := newServerWithMocks(t, nameFailQuerier(t, "volume_labels", upstreamDialErr()), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.NotEqual(t, http.StatusBadGateway, resp.StatusCode)
}

// upstreamTimeoutErr wraps context.DeadlineExceeded the way the promql client
// does, so the cause chain carries the internal upstream URL.
func upstreamTimeoutErr() error {
	return fmt.Errorf(`prom query kube_pod_info: Post "http://vm.internal:8428/api/v1/query": %w`, context.DeadlineExceeded)
}

// TestGraphEndpoint_Timeout504_SanitisedMessage asserts the 504 envelope for a
// timed-out /v1/graph build carries the static build-authored message, not the
// wrapped cause chain embedding the internal VictoriaMetrics URL.
func TestGraphEndpoint_Timeout504_SanitisedMessage(t *testing.T) {
	s := newServerWithMocks(t, newErrQuerier(t, upstreamTimeoutErr()), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)

	var body errReason
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "timeout", body.Error.Reason, "reason string is a contract and must not change")
	assert.Equal(t, "build timeout", body.Error.Message)
	assertNoUpstreamLeak(t, body.Error.Message)
}

// TestMapBuildError_DefaultInternal_SanitisedMessage drives the default-500
// branch of mapBuildError directly: an untyped error (no build.Reason) must
// produce a static "internal error" message, never err.Error().
func TestMapBuildError_DefaultInternal_SanitisedMessage(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/graph", nil)

	s.mapBuildError(c, upstreamDialErr())

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	var body errReason
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "internal", body.Error.Reason, "reason string is a contract and must not change")
	assert.Equal(t, "internal error", body.Error.Message)
	assertNoUpstreamLeak(t, body.Error.Message)
}
