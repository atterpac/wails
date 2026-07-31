package buildsystem

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveDevProducesExactSupervisedProcesses(t *testing.T) {
	root := t.TempDir()
	target := Target{Platform: "windows", Arch: "amd64"}
	plan := &Plan{
		Version: PlanVersion, Mode: "development", Goal: "build",
		Project: Project{Root: root, Config: filepath.Join(root, "build", "config.yml"), BinaryName: "example", Output: "bin", Frontend: "web", PackageManager: "pnpm"},
		Targets: []Target{target},
		Stages: []Stage{{
			ID: "bundle.assemble", Instance: "bundle.assemble[windows/amd64]", Target: &target,
			Outputs: []Artifact{{Name: "bundle", Path: "bin/example.windows"}},
		}},
	}

	dev, err := ResolveDev(plan, DevRequest{
		CLIPath: "C:/tools/wails3.exe", Host: "localhost", Port: 9245,
		Tags: []string{"mcp"},
	})
	require.NoError(t, err)
	require.Len(t, dev.Processes, 3)
	assert.Equal(t, "dev.compile", dev.Processes[0].ID)
	assert.Equal(t, "blocking", dev.Processes[0].Type)
	assert.Equal(t, []string{
		"C:/tools/wails3.exe", "build", "--pipeline", "--config", filepath.Join(root, "build", "config.yml"), "DEV=true", "--tags", "mcp",
	}, dev.Processes[0].Command)
	assert.Equal(t, []string{"pnpm", "dev", "--port", "9245", "--strictPort"}, dev.Processes[1].Command)
	assert.Equal(t, "background", dev.Processes[1].Type)
	require.NotNil(t, dev.Processes[1].Readiness)
	assert.Equal(t, "localhost:9245", dev.Processes[1].Readiness.TCP)
	assert.Equal(t, "30s", dev.Processes[1].Readiness.Timeout)
	assert.Equal(t, "5s", dev.Processes[1].ShutdownTimeout)
	assert.Equal(t, "fail", dev.Processes[1].ExitPolicy)
	assert.Equal(t, []string{filepath.Join(root, "bin", "example.windows", "example.exe")}, dev.Processes[2].Command)
	assert.Equal(t, "primary", dev.Processes[2].Type)
	assert.Equal(t, "5s", dev.Processes[2].ShutdownTimeout)
	assert.Equal(t, "shutdown", dev.Processes[2].ExitPolicy)
	assert.Equal(t, "http://localhost:9245", dev.Processes[2].Environment["FRONTEND_DEVSERVER_URL"])
}

func TestResolveDevUsesPlatformBundleExecutable(t *testing.T) {
	tests := []struct {
		platform string
		bundle   string
		want     string
	}{
		{platform: "darwin", bundle: "bin/example.app", want: filepath.Join("bin", "example.app", "Contents", "MacOS", "example")},
		{platform: "linux", bundle: "bin/example.AppDir", want: filepath.Join("bin", "example.AppDir", "usr", "bin", "example")},
	}
	for _, test := range tests {
		t.Run(test.platform, func(t *testing.T) {
			root := t.TempDir()
			target := Target{Platform: test.platform, Arch: "amd64"}
			plan := &Plan{
				Version: PlanVersion, Mode: "development",
				Project: Project{Root: root, BinaryName: "example", Frontend: "frontend", PackageManager: "npm"},
				Targets: []Target{target},
				Stages:  []Stage{{ID: "bundle.assemble", Target: &target, Outputs: []Artifact{{Name: "bundle", Path: test.bundle}}}},
			}
			dev, err := ResolveDev(plan, DevRequest{CLIPath: "/tools/wails3", Port: 9245})
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(root, test.want), dev.Processes[2].Command[0])
		})
	}
}

func TestResolveDevRejectsMobileTarget(t *testing.T) {
	target := Target{Platform: "android", Arch: "arm64"}
	_, err := ResolveDev(&Plan{Mode: "development", Targets: []Target{target}}, DevRequest{CLIPath: "wails3", Port: 9245})
	require.EqualError(t, err, `typed development mode does not yet support target "android"`)
}
