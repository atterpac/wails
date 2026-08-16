package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wailsapp/wails/v3/internal/buildsystem"
	"github.com/wailsapp/wails/v3/internal/flags"
)

func TestExportBuildConfigReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.reference.yml")
	require.NoError(t, exportBuildConfigReference(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	for _, stage := range []string{
		"project.resolve", "toolchain.check", "dependencies.prepare", "bindings.generate",
		"assets.generate", "frontend.build", "platform.generate", "native.compile",
		"binary.combine", "bundle.assemble", "bundle.sign", "package.create",
		"package.sign", "package.notarize", "artifacts.collect",
	} {
		assert.Contains(t, content, stage+":")
	}

	plan, err := buildsystem.Resolve(buildsystem.Request{ProjectRoot: t.TempDir(), ConfigPath: path})
	require.NoError(t, err)
	assert.NotEmpty(t, plan.Stages)

	err = exportBuildConfigReference(path)
	assert.ErrorContains(t, err, "already exists")
}

func TestBuildConfigExportRejectsBuildArguments(t *testing.T) {
	err := Build(&flags.Build{ConfigExport: true}, []string{"GOOS=linux"})
	assert.EqualError(t, err, "--config-export does not accept build arguments")
}
