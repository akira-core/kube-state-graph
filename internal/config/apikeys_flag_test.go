package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --api-keys on the command line is recorded; KSG_API_KEYS is not.
func TestParse_APIKeysFromFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want bool
	}{
		{"double-dash equals", []string{"--api-keys=k1"}, nil, true},
		{"single-dash separate value", []string{"-api-keys", "k1"}, nil, true},
		{"empty flag value still counts", []string{"--api-keys="}, nil, true},
		{"env only", nil, map[string]string{"KSG_API_KEYS": "k1"}, false},
		{"nothing", nil, nil, false},
		{"other flags only", []string{"--api-keys-file=/keys"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse(tc.args, func(k string) (string, bool) {
				v, ok := tc.env[k]
				return v, ok
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.APIKeysFromFlag)
		})
	}
}
