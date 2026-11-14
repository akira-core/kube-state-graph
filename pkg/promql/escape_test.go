package promql

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEscapeLiteral(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"plain":           {"shop", "shop"},
		"unicode":         {"café-東京", "café-東京"},
		"quote":           {`a"b`, `a\"b`},
		"backslash":       {`a\b`, `a\\b`},
		"regex-quoted":    {`prod\.eu`, `prod\\.eu`},
		"newline":         {"a\nb", `a\nb`},
		"carriage return": {"a\rb", `a\rb`},
		"tab":             {"a\tb", `a\tb`},
		"nul":             {"a\x00b", `a\x00b`},
		"escape":          {"a\x1bb", `a\x1bb`},
		"unit separator":  {"a\x1fb", `a\x1fb`},
		"del":             {"a\x7fb", `a\x7fb`},
		"mixed":           {"\"x\"\n\\", `\"x\"\n\\`},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, escapeLiteral(tc.in))
		})
	}
}

// Every escaped value is a valid Go-style double-quoted literal — the escape
// grammar PromQL and MetricsQL share — that decodes back to the input, and it
// never carries a raw control byte or an unescaped quote.
func TestEscapeLiteral_RoundTripsEveryASCIIByte(t *testing.T) {
	var all strings.Builder
	for b := range 0x80 {
		all.WriteByte(byte(b))
	}
	all.WriteString("é東")
	in := all.String()

	out := escapeLiteral(in)
	got, err := strconv.Unquote(`"` + out + `"`)
	require.NoError(t, err)
	assert.Equal(t, in, got)
	for i := range len(out) {
		assert.False(t, out[i] < 0x20 || out[i] == 0x7f, "raw control byte %#x in output", out[i])
	}
}

// A value needing no escape is returned without a copy.
func TestEscapeLiteral_NoEscapeNoAllocation(t *testing.T) {
	s := "kube-system"
	allocs := testing.AllocsPerRun(100, func() { _ = escapeLiteral(s) })
	assert.Zero(t, allocs)
}
