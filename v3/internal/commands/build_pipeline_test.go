package commands

import (
	"context"
	"encoding/json"
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
		"WAILS_BUILD_CONTEXT": filepath.Join(root, ".wails", "build", "context", "native-compile-windows-amd64.json"),
		"WAILS_BUILD_STAGE":   "native.compile[windows/amd64]",
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

func TestResolveBuildActionsExpandsEveryMatrixTarget(t *testing.T) {
	root := t.TempDir()
	plan, err := buildsystem.Resolve(buildsystem.Request{
		ProjectRoot: root,
		Targets: []buildsystem.Target{
			{Platform: "windows", Arch: "amd64"},
			{Platform: "linux", Arch: "arm64"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))

	var native []buildsystem.Stage
	var collect buildsystem.Stage
	for _, stage := range plan.Stages {
		switch stage.ID {
		case "native.compile":
			native = append(native, stage)
		case "artifacts.collect":
			collect = stage
		}
	}
	require.Len(t, native, 2)
	assert.Equal(t, "native.compile[windows/amd64]", native[0].Reference())
	assert.Contains(t, native[0].Actions[2].Command, filepath.Join(root, "bin", "windows", "amd64", filepath.Base(root)+".exe"))
	assert.Equal(t, "windows", native[0].Actions[2].Environment["GOOS"])
	assert.Equal(t, "amd64", native[0].Actions[2].Environment["GOARCH"])
	assert.Equal(t, "native.compile[linux/arm64]", native[1].Reference())
	assert.Contains(t, native[1].Actions[1].Command, filepath.Join(root, "bin", "linux", "arm64", filepath.Base(root)))
	assert.Equal(t, "linux", native[1].Actions[1].Environment["GOOS"])
	assert.Equal(t, "arm64", native[1].Actions[1].Environment["GOARCH"])

	require.Len(t, collect.Actions, 1)
	var inputs []collectionInput
	require.NoError(t, json.Unmarshal([]byte(collect.Actions[0].Parameters["binaries"]), &inputs))
	require.Len(t, inputs, 2)
	assert.Equal(t, "binary[windows/amd64]", inputs[0].ID)
	assert.Equal(t, "binary[linux/arm64]", inputs[1].ID)
}

func TestCollectArtifactsWritesEveryMatrixBinary(t *testing.T) {
	root := t.TempDir()
	windowsPath := filepath.Join(root, "bin", "windows", "amd64", "app.exe")
	linuxPath := filepath.Join(root, "bin", "linux", "arm64", "app")
	require.NoError(t, os.MkdirAll(filepath.Dir(windowsPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(linuxPath), 0o755))
	require.NoError(t, os.WriteFile(windowsPath, []byte("windows"), 0o755))
	require.NoError(t, os.WriteFile(linuxPath, []byte("linux"), 0o755))
	inputs := []collectionInput{
		{
			ID:         "binary[windows/amd64]",
			Type:       "native-binary",
			Path:       windowsPath,
			ReportPath: "bin/windows/amd64/app.exe",
			Platform:   "windows",
			Arch:       "amd64",
		},
		{
			ID:         "binary[linux/arm64]",
			Type:       "native-binary",
			Path:       linuxPath,
			ReportPath: "bin/linux/arm64/app",
			Platform:   "linux",
			Arch:       "arm64",
		},
	}
	encoded, err := json.Marshal(inputs)
	require.NoError(t, err)
	manifestPath := filepath.Join(root, "bin", "artifacts.json")
	stage := &buildsystem.StageContext{
		Plan:  &buildsystem.Plan{Project: buildsystem.Project{Root: root}},
		Stage: buildsystem.Stage{ID: "artifacts.collect", Instance: "artifacts.collect"},
		Artifacts: map[string]buildsystem.Artifact{
			inputs[0].ID: {ID: inputs[0].ID, Name: "binary"},
			inputs[1].ID: {ID: inputs[1].ID, Name: "binary"},
		},
	}
	require.NoError(t, collectArtifactsAction(context.Background(), stage, buildsystem.Action{
		Parameters: map[string]string{
			"binaries": string(encoded),
			"manifest": manifestPath,
		},
	}))

	data, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	var manifest buildArtifactManifest
	require.NoError(t, json.Unmarshal(data, &manifest))
	require.Len(t, manifest.Artifacts, 2)
	assert.Equal(t, "windows", manifest.Artifacts[0].Platform)
	assert.Equal(t, "bin/windows/amd64/app.exe", manifest.Artifacts[0].Path)
	assert.NotEmpty(t, manifest.Artifacts[0].SHA256)
	assert.Equal(t, "linux", manifest.Artifacts[1].Platform)
}
