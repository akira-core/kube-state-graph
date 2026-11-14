package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactURL(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"http://vm.example:8428", "http://vm.example:8428"},
		{"http://ksg:s3cret@vm.example:8428/select/0", "http://ksg:xxxxx@vm.example:8428/select/0"},
		{"https://ksg@vm.example", "https://ksg@vm.example"},
		{"//ksg:s3cret@vm.example:8428", "//ksg:xxxxx@vm.example:8428"},
		// Opaque: scheme "ksg", opaque "s3cret@vm.example:8428".
		{"ksg:s3cret@vm.example:8428", UnparseableURL},
		// Unparseable: url.Parse fails and its error would quote the input.
		{"http://ksg:s3cret@vm.example:84x28", UnparseableURL},
		{"http://ksg:s3%zzcret@vm.example", UnparseableURL},
	} {
		got := RedactURL(tc.in)
		assert.Equal(t, tc.want, got, "RedactURL(%q)", tc.in)
		assert.NotContains(t, got, "s3cret")
	}
}

// The startup validation error never carries the password of a rejected
// --prom-url.
func TestValidate_InvalidPromURLErrorIsRedacted(t *testing.T) {
	for _, raw := range []string{
		"ksg:s3cret@vm.example:8428",          // opaque, no host
		"http://ksg:s3cret@vm.example:84x28",  // unparseable port
		"http://ksg:s3cret@",                  // no host
		"//ksg:s3cret@vm.example:8428/select", // no scheme
	} {
		cfg := Defaults()
		cfg.PromURL = raw
		err := cfg.Validate()
		require.Error(t, err, raw)
		assert.Contains(t, err.Error(), "prom-url is not a valid URL", raw)
		assert.NotContains(t, err.Error(), "s3cret", raw)
	}
}
