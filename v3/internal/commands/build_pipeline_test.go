package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wailsapp/wails/v3/internal/buildsystem"
	"github.com/wailsapp/wails/v3/internal/flags"
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

func TestResolveBuildActionsExpandsNativeCompile(t *testing.T) {
	root := t.TempDir()
	plan, err := buildsystem.Resolve(buildsystem.Request{
		ProjectRoot: root,
		Target:      "windows",
		Arch:        "amd64",
	})
	require.NoError(t, err)
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))

	var native buildsystem.Stage
	for _, stage := range plan.Stages {
		if stage.ID == "native.compile" {
			native = stage
			break
		}
	}
	require.Equal(t, "native.compile", native.ID)
	require.Len(t, native.Actions, 4)

	assert.Equal(t, buildsystem.ActionMkdir, native.Actions[0].Kind)
	assert.Equal(t, buildsystem.ActionCopy, native.Actions[1].Kind)
	assert.Equal(t, buildsystem.ActionCommand, native.Actions[2].Kind)
	assert.Equal(t, []string{
		"go",
		"build",
		"-tags",
		"production",
		"-trimpath",
		"-buildvcs=false",
		"-ldflags=-w -s",
		"-o",
		filepath.Join(root, "bin", filepath.Base(root)+".exe"),
	}, native.Actions[2].Command)
	assert.Equal(t, map[string]string{
		"CGO_ENABLED":         "0",
		"GOARCH":              "amd64",
		"GOOS":                "windows",
		"WAILS_BUILD_CONTEXT": filepath.Join(root, ".wails", "build", "context", "native-compile.json"),
		"WAILS_BUILD_STAGE":   "native.compile",
	}, native.Actions[2].Environment)
	assert.Equal(t, buildsystem.ActionRemove, native.Actions[3].Kind)
	assert.True(t, native.Actions[3].Finally)
}

func TestInspectedActionsAreExecutedWithoutReconstruction(t *testing.T) {
	root := t.TempDir()
	plan, err := buildsystem.Resolve(buildsystem.Request{
		ProjectRoot: root,
		Target:      "linux",
		Arch:        "amd64",
	})
	require.NoError(t, err)
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))
	inspection, err := buildsystem.InspectStage(plan, "project.resolve")
	require.NoError(t, err)

	var executed []buildsystem.Action
	require.NoError(t, buildsystem.Execute(context.Background(), plan, buildsystem.ExecuteOptions{
		Step: "project.resolve",
		OnAction: func(_ buildsystem.Stage, action buildsystem.Action, status string) {
			if status == "completed" {
				executed = append(executed, action)
			}
		},
	}))
	assert.Equal(t, inspection.Stage.Actions, executed)
}
