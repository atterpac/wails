package commands

import (
	"path/filepath"
	"testing"

	"github.com/atterpac/refresh/engine"
	"github.com/atterpac/refresh/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wailsapp/wails/v3/internal/buildsystem"
	"github.com/wailsapp/wails/v3/internal/flags"
)

func TestRefreshConfigForDevPreservesWatchAndReplacesLegacyProcesses(t *testing.T) {
	configured := engine.Config{
		RootPath: ".", Debounce: 750,
		Ignore:           engine.Ignore{Dir: []string{"frontend"}, WatchedExten: []string{"*.go"}},
		ExecList:         []string{"legacy"},
		ExecStruct:       []process.Execute{{Cmd: "wails3 task build", Type: process.Blocking}},
		BackgroundStruct: process.Execute{Cmd: "legacy background"},
	}
	plan := &buildsystem.DevPlan{Processes: []buildsystem.DevProcess{
		{ID: "dev.compile", Type: "blocking", Command: []string{"wails3", "build", "--pipeline"}, WorkingDirectory: "/project", Environment: map[string]string{"DEV": "true"}},
		{ID: "dev.launch", Type: "primary", Command: []string{"/project/bin/app"}, WorkingDirectory: "/project", ShutdownTimeout: "5s", ExitPolicy: "shutdown",
			Readiness: &buildsystem.DevReadiness{TCP: "localhost:9245", Timeout: "30s", Interval: "100ms"}},
	}}

	result := refreshConfigForDev(configured, plan)
	assert.Equal(t, 750, result.Debounce)
	assert.Equal(t, []string{"frontend"}, result.Ignore.Dir)
	assert.Nil(t, result.ExecList)
	assert.Empty(t, result.BackgroundStruct.Cmd)
	require.Len(t, result.ExecStruct, 2)
	assert.Equal(t, "dev.compile", result.ExecStruct[0].Name)
	assert.Equal(t, []string{"wails3", "build", "--pipeline"}, result.ExecStruct[0].Command)
	assert.Equal(t, map[string]string{"DEV": "true"}, result.ExecStruct[0].Env)
	require.NotNil(t, result.ExecStruct[1].Readiness)
	assert.Equal(t, "localhost:9245", result.ExecStruct[1].Readiness.TCP)
	assert.Equal(t, "5s", result.ExecStruct[1].ShutdownTimeout)
	assert.Equal(t, process.ExitPolicyShutdown, result.ExecStruct[1].ExitPolicy)
	assert.Contains(t, result.Ignore.File, "*_test.go")
}

func TestDevelopmentBuildActionsUseDevelopmentFrontendAndCompiler(t *testing.T) {
	root := t.TempDir()
	plan, err := buildsystem.Resolve(buildsystem.Request{
		ProjectRoot: root, Target: "linux", Arch: "amd64", Mode: "development",
	})
	require.NoError(t, err)
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))

	frontend := findBuildStage(t, plan, "frontend.build")
	var frontendCommand buildsystem.Action
	for _, action := range frontend.Actions {
		if action.Kind == buildsystem.ActionCommand && len(action.Command) >= 3 && action.Command[1] == "run" {
			frontendCommand = action
		}
	}
	assert.Equal(t, []string{plan.Project.PackageManager, "run", "build:dev"}, frontendCommand.Command)
	assert.Equal(t, "false", frontendCommand.Environment["PRODUCTION"])

	native := findBuildStage(t, plan, "native.compile")
	bindings := findBuildStage(t, plan, "bindings.generate")
	require.Len(t, bindings.Actions, 1)
	assert.Empty(t, bindings.Actions[0].Parameters["buildFlags"])
	var compile buildsystem.Action
	for _, action := range native.Actions {
		if action.Kind == buildsystem.ActionCommand && len(action.Command) > 1 && action.Command[0] == "go" && action.Command[1] == "build" {
			compile = action
		}
	}
	assert.Contains(t, compile.Command, "-gcflags=all=-l")
	assert.NotContains(t, compile.Command, "-trimpath")
	assert.NotContains(t, compile.Command, "-ldflags=-w -s")
	assert.Equal(t, filepath.Join(root, "bin", filepath.Base(root)), compile.Command[len(compile.Command)-1])
}

func TestDevelopmentDarwinBundleUsesDevelopmentPlistAndAdHocSigning(t *testing.T) {
	root := t.TempDir()
	plan, err := buildsystem.Resolve(buildsystem.Request{
		ProjectRoot: root, Target: "darwin", Arch: "arm64", Mode: "development",
	})
	require.NoError(t, err)
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))

	platform := findBuildStage(t, plan, "platform.generate")
	var plist buildsystem.Action
	for _, action := range platform.Actions {
		if action.Kind == buildsystem.ActionCopy && filepath.Base(action.Destination) == "Info.plist" {
			plist = action
		}
	}
	assert.Equal(t, filepath.Join(root, "build", "darwin", "Info.dev.plist"), plist.Source)

	bundle := findBuildStage(t, plan, "bundle.assemble")
	require.NotEmpty(t, bundle.Actions)
	signing := bundle.Actions[len(bundle.Actions)-1]
	assert.Equal(t, []string{"codesign", "--force", "--deep", "--sign", "-", filepath.Join(root, "bin", filepath.Base(root)+".app")}, signing.Command)
}
