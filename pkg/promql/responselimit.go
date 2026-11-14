package promql

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// DefaultMaxResponseBytes bounds one upstream response body when no
// WithMaxResponseBytes option is given: 512 MiB, an order of magnitude above
// the largest leg the default query-cache budget is sized for, so it only ever
// stops a runaway or hostile store from exhausting the process's memory.
const DefaultMaxResponseBytes int64 = 512 << 20

// ErrResponseTooLarge is the error an upstream query fails with when the
// response body exceeds the configured bound. It is an ordinary upstream query
// error: a required leg fails closed, an optional one degrades, nothing is
// cached.
var ErrResponseTooLarge = errors.New("upstream response exceeds the size bound")

// WithMaxResponseBytes bounds every response body the client reads. n <= 0
// disables the bound; without this option DefaultMaxResponseBytes applies.
func WithMaxResponseBytes(n int64) Option {
	return func(o *clientOptions) {
		o.maxResponseBytes = max(0, n)
		o.maxResponseBytesSet = true
	}
}

// responseBound resolves the configured bound: the option's value when one was
// given (0 = unbounded), DefaultMaxResponseBytes otherwise.
func (o clientOptions) responseBound() int64 {
	if o.maxResponseBytesSet {
		return o.maxResponseBytes
	}
	return DefaultMaxResponseBytes
}

// responseLimitTransport refuses a response whose declared length exceeds max
// and wraps every other body so reading past max fails rather than truncates.
// A truncated body would surface as a JSON decoding error naming nothing,
// where this names the bound.
type responseLimitTransport struct {
	inner http.RoundTripper
	max   int64
}

func (t *responseLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.max {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: declared %d bytes, bound %d", ErrResponseTooLarge, resp.ContentLength, t.max)
	}
	resp.Body = &limitedBody{rc: resp.Body, remaining: t.max, max: t.max}
	return resp, nil
}

// limitedBody passes through at most max bytes; once they are consumed, a read
// that finds more data fails with ErrResponseTooLarge, while a body that ends
// exactly at the bound reads to a clean EOF.
type limitedBody struct {
	rc        io.ReadCloser
	remaining int64
	max       int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var probe [1]byte
		n, err := b.rc.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("%w: more than %d bytes", ErrResponseTooLarge, b.max)
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }
