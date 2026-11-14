package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// TestBuildResource_ExcludesCommandLine pins the process attributes the
// resource carries. argv can hold --api-keys or a DSN with userinfo, so
// process.command_args must never be detected; the executable path and the OS
// owner are dropped with it. process.pid and process.runtime.name are asserted
// PRESENT so the test fails if process detection stops running altogether.
func TestBuildResource_ExcludesCommandLine(t *testing.T) {
	clearOTELEnv(t)

	res, err := buildResource(t.Context(), "test")
	require.NoError(t, err)

	keys := make(map[attribute.Key]struct{}, res.Len())
	for _, kv := range res.Attributes() {
		keys[kv.Key] = struct{}{}
	}

	for _, banned := range []attribute.Key{
		semconv.ProcessCommandArgsKey,
		semconv.ProcessExecutablePathKey,
		semconv.ProcessOwnerKey,
	} {
		assert.NotContains(t, keys, banned, "resource must not carry %s", banned)
	}
	for _, kept := range []attribute.Key{
		semconv.ProcessPIDKey,
		semconv.ProcessExecutableNameKey,
		semconv.ProcessRuntimeNameKey,
		semconv.ProcessRuntimeVersionKey,
		semconv.ProcessRuntimeDescriptionKey,
		semconv.ServiceNameKey,
	} {
		assert.Contains(t, keys, kept, "resource must carry %s", kept)
	}
}
