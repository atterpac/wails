package buildsystem

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveBuildPlan(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "web"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "web", "pnpm-lock.yaml"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build", "config.yml"), []byte(`
version: "3"
info:
  productName: Plan Test
build:
  binaryName: plan-test
  output: dist
  tags: [enterprise]
  frontend:
    directory: web
    output: web/public
  stages:
    native.compile:
      before:
        - name: generate
          command: [go, generate, ./...]
          scope: build
      after:
        - command: [./scripts/report]
    artifacts.collect:
      replace:
        command: [./scripts/collect]
        args: ["--output", dist/release.json]
        produces:
          manifest: dist/release.json
`), 0o644))

	plan, err := Resolve(Request{
		ProjectRoot: root,
		Target:      "windows",
		Arch:        "arm64",
		Tags:        []string{"custom,enterprise"},
		Obfuscated:  true,
	})
	require.NoError(t, err)

	assert.Equal(t, PlanVersion, plan.Version)
	assert.Equal(t, "Plan Test", plan.Project.Name)
	assert.Equal(t, "plan-test", plan.Project.BinaryName)
	assert.Equal(t, "dist", plan.Project.Output)
	assert.Equal(t, "web", plan.Project.Frontend)
	assert.Equal(t, "pnpm", plan.Project.PackageManager)
	assert.Equal(t, "windows", plan.Target.Platform)
	assert.Equal(t, "arm64", plan.Target.Arch)
	assert.Equal(t, []string{"enterprise", "custom", "production", "wails_obfuscated"}, plan.Target.Tags)
	assert.Empty(t, plan.Diagnostics)

	native := findStage(t, plan, "native.compile")
	assert.Equal(t, "planned", native.Status)
	assert.Equal(t, "dist/plan-test.exe", native.Outputs[0].Path)
	assert.ElementsMatch(t, []string{"frontend.build", "platform.generate"}, native.Needs)
	require.Len(t, native.Before, 1)
	assert.Equal(t, []string{"go", "generate", "./..."}, native.Before[0].Command)
	require.Len(t, native.After, 1)
	assert.Equal(t, []string{"./scripts/report"}, native.After[0].Command)
	assert.Equal(t, "stage", native.After[0].Scope)
	assert.Equal(t, "${project.root}", native.After[0].WorkingDirectory)

	bundle := findStage(t, plan, "bundle.assemble")
	assert.Equal(t, "skipped", bundle.Status)
	assert.Equal(t, "not selected by the build goal", bundle.Reason)

	collect := findStage(t, plan, "artifacts.collect")
	assert.Equal(t, "command", collect.Implementation)
	require.NotNil(t, collect.Replacement)
	assert.Equal(t, []string{"./scripts/collect"}, collect.Replacement.Command)
	assert.Equal(t, "dist/release.json", collect.Outputs[0].Path)
}

func TestResolveUsesDefaultsWithoutConfig(t *testing.T) {
	root := t.TempDir()
	plan, err := Resolve(Request{ProjectRoot: root, Target: "linux", Arch: "amd64"})
	require.NoError(t, err)

	assert.Equal(t, filepath.Base(root), plan.Project.Name)
	assert.Equal(t, normaliseName(filepath.Base(root)), plan.Project.BinaryName)
	assert.Equal(t, "bin", plan.Project.Output)
	assert.Equal(t, "frontend", plan.Project.Frontend)
	assert.Equal(t, "npm", plan.Project.PackageManager)
	assert.Equal(t, []string{"production"}, plan.Target.Tags)
	require.Len(t, plan.Diagnostics, 1)
}

func TestResolveRejectsUnsupportedTarget(t *testing.T) {
	_, err := Resolve(Request{ProjectRoot: t.TempDir(), Target: "plan9", Arch: "amd64"})
	require.EqualError(t, err, `unsupported build target "plan9"`)
}

func TestResolveRejectsUnknownConfiguredStage(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build", "config.yml"), []byte(`
build:
  stages:
    typo.compile:
      before:
        - command: [echo, hello]
`), 0o644))

	_, err := Resolve(Request{ProjectRoot: root, Target: "linux", Arch: "amd64"})
	require.EqualError(t, err, `build configuration references unknown stage "typo.compile"`)
}

func TestResolveRejectsReplacementWithoutArtifacts(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build", "config.yml"), []byte(`
build:
  stages:
    native.compile:
      replace:
        command: [./compile]
`), 0o644))

	_, err := Resolve(Request{ProjectRoot: root, Target: "linux", Arch: "amd64"})
	require.EqualError(t, err, `replacement for stage "native.compile" declares no produced artifacts`)
}

func TestResolveEnforcesReplacementArtifactContract(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build", "config.yml"), []byte(`
build:
  stages:
    native.compile:
      replace:
        command: [./compile]
        produces:
          executable: bin/application
`), 0o644))

	_, err := Resolve(Request{ProjectRoot: root, Target: "linux", Arch: "amd64"})
	require.EqualError(t, err, `replacement for stage "native.compile" does not produce required artifact "binary"`)
}

func findStage(t *testing.T, plan *Plan, id string) Stage {
	t.Helper()
	for _, stage := range plan.Stages {
		if stage.ID == id {
			return stage
		}
	}
	t.Fatalf("stage %q not found", id)
	return Stage{}
}
