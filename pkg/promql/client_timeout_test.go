package promql

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluationTimeout(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for name, tc := range map[string]struct {
		left    time.Duration
		bounded bool
		want    time.Duration
		ok      bool
	}{
		"build deadline":         {14900 * time.Millisecond, true, 14 * time.Second, true},
		"exact seconds":          {15500 * time.Millisecond, true, 15 * time.Second, true},
		"minutes":                {2*time.Minute + 700*time.Millisecond, true, 2 * time.Minute, true},
		"just over one second":   {1500 * time.Millisecond, true, time.Second, true},
		"too little left":        {1200 * time.Millisecond, true, 0, false},
		"deadline already past":  {-time.Second, true, 0, false},
		"no deadline":            {0, false, 0, false},
		"no deadline, long left": {time.Hour, false, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := evaluationTimeout(now.Add(tc.left), tc.bounded, now)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// timeoutCapture is a minimal instant-query endpoint recording each request's
// `timeout` form value.
type timeoutCapture struct {
	mu       sync.Mutex
	timeouts []string
	present  []bool
}

func (c *timeoutCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	c.mu.Lock()
	c.timeouts = append(c.timeouts, r.Form.Get("timeout"))
	c.present = append(c.present, r.Form.Has("timeout"))
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
}

func TestClient_SendsEvaluationTimeoutFromDeadline(t *testing.T) {
	capture := &timeoutCapture{}
	srv := httptest.NewServer(capture)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = c.Instant(ctx, string(QPodInfo), "kube_pod_info", ts0)
	require.NoError(t, err)

	_, err = c.Instant(context.Background(), string(QPodInfo), "kube_pod_info", ts0)
	require.NoError(t, err)

	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Len(t, capture.timeouts, 2)
	got, err := time.ParseDuration(capture.timeouts[0])
	require.NoError(t, err)
	assert.Equal(t, got.Truncate(time.Second), got, "whole seconds only")
	assert.GreaterOrEqual(t, got, 8*time.Second)
	assert.LessOrEqual(t, got, 9*time.Second)
	assert.False(t, capture.present[1], "no deadline, no timeout parameter")
}

// Through the guard, a coalesced fetch sends the timeout of the waiter that
// will wait longest, not of the caller that started it.
func TestGuard_CoalescedFetchSendsLatestWaiterTimeout(t *testing.T) {
	capture := &timeoutCapture{}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		capture.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	c, err := New(srv.URL, nil)
	require.NoError(t, err)
	// A slot limit of one with a holder makes the shared fetch wait for its
	// slot until the second waiter has joined, so the request is sent after
	// the deadline was widened.
	gate := newGate()
	split := &routeByQuery{byQuery: map[string]Querier{"holder": gate}, other: c}
	q := Guard(split, WithMaxConcurrency(1))
	go func() { _, _ = q.Instant(context.Background(), string(QPodInfo), "holder", ts0) }()
	<-gate.started

	short, cancelShort := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancelShort()
	go func() { _, _ = q.Instant(short, string(QPodInfo), "kube_pod_info", ts0) }()
	waitForWaiters(t, q, "kube_pod_info", 1)
	long, cancelLong := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelLong()
	done := make(chan error, 1)
	go func() {
		_, err := q.Instant(long, string(QPodInfo), "kube_pod_info", ts0)
		done <- err
	}()
	waitForWaiters(t, q, "kube_pod_info", 2)
	close(gate.release)
	close(release)
	require.NoError(t, <-done)

	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Len(t, capture.timeouts, 1)
	got, err := time.ParseDuration(capture.timeouts[0])
	require.NoError(t, err)
	assert.Greater(t, got, 20*time.Second, "the latest waiter's deadline, not the starter's 3s")
}

// routeByQuery sends listed query strings to their own querier and the rest
// to other (test-only).
type routeByQuery struct {
	byQuery map[string]Querier
	other   Querier
}

func (r *routeByQuery) Instant(ctx context.Context, name, query string, ts time.Time) (model.Vector, error) {
	if q, ok := r.byQuery[query]; ok {
		return q.Instant(ctx, name, query, ts)
	}
	return r.other.Instant(ctx, name, query, ts)
}
