package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestUpdateSigningConfigUsesTypedBuildConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	require.NoError(t, os.WriteFile(path, []byte("info:\n  productName: Example\nbuild:\n  binaryName: example\n"), 0o644))

	require.NoError(t, updateSigningConfig(path, "windows", map[string]string{
		"certificate": "certs/release.pfx", "thumbprint": "", "timestampServer": "https://timestamp.example.com",
	}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var config map[string]any
	require.NoError(t, yaml.Unmarshal(data, &config))
	build := config["build"].(map[string]any)
	assert.Equal(t, "example", build["binaryName"])
	windows := build["signing"].(map[string]any)["windows"].(map[string]any)
	assert.Equal(t, "certs/release.pfx", windows["certificate"])
	assert.Equal(t, "https://timestamp.example.com", windows["timestampServer"])
	assert.NotContains(t, windows, "thumbprint")
}
