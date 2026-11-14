package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Spec: "A well-formed id is kept" and "An oversized or malformed id is
// replaced".
func TestRequestID_IsBoundedAndCharsetRestricted(t *testing.T) {
	s := newServerWithMocks(t, newMockQuerier(t, nil), nil)
	var buf bytes.Buffer
	s.logger = slog.New(slog.NewJSONHandler(&buf, nil))
	h := s.Handler()

	cases := []struct {
		name string
		in   string
		keep bool
	}{
		{"uuid", "3f1c2b8e-9a4d-4b1e-8c2f-0d7e5a6b9c10", true},
		{"every allowed byte", "AZaz09._:=+/@-", true},
		{"exactly 128 bytes", strings.Repeat("a", 128), true},
		{"129 bytes", strings.Repeat("a", 129), false},
		{"space", "abc def", false},
		{"quote", `abc"def`, false},
		{"newline", "abc\ndef", false},
		{"semicolon", "Root=1-abc;Parent=2", false},
		{"non-ascii", "abcé", false},
		{"absent", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			req := httptest.NewRequest(http.MethodGet, "/v1/graph?start=1746442800&end=1746446400", nil)
			if tc.in != "" {
				req.Header["X-Request-Id"] = []string{tc.in}
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)

			got := rec.Header().Get("X-Request-ID")
			logged := accessLogLine(t, &buf)["request_id"]
			assert.Equal(t, got, logged, "the header and the log carry one id")
			if tc.keep {
				assert.Equal(t, tc.in, got)
				return
			}
			_, err := uuid.Parse(got)
			assert.NoError(t, err, "a rejected id is replaced by a generated UUID, got %q", got)
			if tc.in != "" {
				assert.NotContains(t, buf.String(), tc.in, "the inbound value is not logged")
			}
		})
	}
}
