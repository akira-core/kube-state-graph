package store

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A DSN that does not parse as a URL must not reach an error message, a log
// line or a startup failure verbatim — nor any fragment of its userinfo. The
// URL parser's own error embeds the whole input, and even its inner error
// quotes pieces of it: a password containing '/' ends the authority early and
// surfaces as an "invalid port" quoting the password's head; a bad '%' escape
// is quoted as-is.
func TestParseDSN_MalformedURLNeverEchoesTheDSN(t *testing.T) {
	cases := []struct {
		name, dsn string
		secrets   []string
	}{
		{"slash_in_password", "clickhouse://ksg:s3cr/et@ch.example:9000/routes", []string{"s3cr", "et@ch", "ksg:s3cr"}},
		{"bad_percent_escape", "clickhouse://ksg:pa%zzword@ch.example:9000/routes", []string{"pa%zz", "%zzw", "word"}},
		{"control_character", "clickhouse://ksg:hunter2\x7f@ch.example:9000/routes", []string{"hunter2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseDSN(c.dsn)
			require.Error(t, err)
			msg := err.Error()
			assert.NotContains(t, msg, c.dsn)
			for _, s := range c.secrets {
				assert.NotContains(t, msg, s)
			}
			assert.Contains(t, msg, "route-store dsn")
		})
	}
}

// A DSN that parses keeps its credentials for the dial; errors that are not
// URL-syntax errors (a bad query parameter) still say which parameter.
func TestParseDSN_ValidAndParameterErrors(t *testing.T) {
	o, err := parseDSN("clickhouse://ksg:s3cret@ch.example:9000/routes")
	require.NoError(t, err)
	assert.Equal(t, "ksg", o.Auth.Username)
	assert.Equal(t, "s3cret", o.Auth.Password)

	_, err = parseDSN("clickhouse://ch.example:9000/routes?dial_timeout=soon")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dial timeout")
}

func TestPlaintextCredentials(t *testing.T) {
	plain, err := parseDSN("clickhouse://ksg:s3cret@ch.example:9000/routes")
	require.NoError(t, err)
	assert.True(t, plaintextCredentials(plain), "password over a non-TLS connection")

	secure, err := parseDSN("clickhouse://ksg:s3cret@ch.example:9440/routes?secure=true")
	require.NoError(t, err)
	assert.False(t, plaintextCredentials(secure), "TLS protects the password")

	anon, err := parseDSN("clickhouse://ch.example:9000/routes")
	require.NoError(t, err)
	assert.False(t, plaintextCredentials(anon), "no password, nothing exposed")

	envAuth := &clickhouse.Options{}
	applyAuth(envAuth, openConfig{username: "ksg", password: "from-env"})
	assert.True(t, plaintextCredentials(envAuth), "env credentials count too")
}

// Open warns before dialling when a password would cross the wire in the
// clear, and the warning carries neither credential. The dial itself fails
// here (nothing listens), which is irrelevant to the warning.
func TestOpen_WarnsOnPlaintextCredentialsWithoutValues(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, _ = Open(ctx, "clickhouse://ksg:s3cret@127.0.0.1:1/routes?dial_timeout=50ms")

	logged := buf.String()
	assert.Contains(t, logged, "without TLS")
	assert.Contains(t, logged, "route_store_tls=false")
	assert.NotContains(t, logged, "s3cret")
	assert.NotContains(t, logged, "ksg:", "no userinfo in the log")
}
