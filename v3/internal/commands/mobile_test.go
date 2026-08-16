package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMobileProjectReadsTypedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	require.NoError(t, os.WriteFile(path, []byte(`
info:
  productName: Example App
  productIdentifier: com.example.app
build:
  binaryName: example
  output: artifacts
`), 0o644))
	identity, binary, output, err := mobileProject(path)
	require.NoError(t, err)
	assert.Equal(t, "com.example.app", identity)
	assert.Equal(t, "example", binary)
	assert.Equal(t, "artifacts", output)
}

func TestDevTargetDefaultsMobileSimulatorArchitecture(t *testing.T) {
	platform, arch, err := devTarget("android", "")
	require.NoError(t, err)
	assert.Equal(t, "android", platform)
	assert.Equal(t, "amd64", arch)
	platform, arch, err = devTarget("ios/arm64", "")
	require.NoError(t, err)
	assert.Equal(t, "ios", platform)
	assert.Equal(t, "arm64", arch)
	platform, arch, err = devTarget("ios", "physical-device")
	require.NoError(t, err)
	assert.Equal(t, "ios", platform)
	assert.Equal(t, "arm64", arch)
}
