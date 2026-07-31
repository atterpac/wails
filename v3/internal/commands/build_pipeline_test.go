package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrontendInstallCommand(t *testing.T) {
	frontend := t.TempDir()

	name, args := frontendInstallCommand(frontend, "npm")
	assert.Equal(t, "npm", name)
	assert.Equal(t, []string{"install", "--no-package-lock"}, args)

	require.NoError(t, os.WriteFile(filepath.Join(frontend, "package-lock.json"), nil, 0o644))
	name, args = frontendInstallCommand(frontend, "npm")
	assert.Equal(t, "npm", name)
	assert.Equal(t, []string{"ci"}, args)

	require.NoError(t, os.WriteFile(filepath.Join(frontend, "pnpm-lock.yaml"), nil, 0o644))
	name, args = frontendInstallCommand(frontend, "pnpm")
	assert.Equal(t, "pnpm", name)
	assert.Equal(t, []string{"install", "--frozen-lockfile"}, args)
}
