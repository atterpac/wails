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
	require.Len(t, plan.Targets, 1)
	assert.Equal(t, "windows", plan.Targets[0].Platform)
	assert.Equal(t, "arm64", plan.Targets[0].Arch)
	assert.Equal(t, []string{"enterprise", "custom", "production", "wails_obfuscated"}, plan.Targets[0].Tags)
	assert.Empty(t, plan.Diagnostics)

	native := findStage(t, plan, "native.compile")
	assert.Equal(t, "native.compile[windows/arm64]", native.Instance)
	assert.Equal(t, "planned", native.Status)
	assert.Equal(t, "dist/plan-test.exe", native.Outputs[0].Path)
	assert.ElementsMatch(t, []string{"frontend.build", "platform.generate[windows/arm64]"}, native.Needs)
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
	assert.Equal(t, []string{"production"}, plan.Targets[0].Tags)
	require.Len(t, plan.Diagnostics, 1)
}

func TestResolveExpandsTargetMatrix(t *testing.T) {
	root := t.TempDir()
	plan, err := Resolve(Request{
		ProjectRoot: root,
		Targets: []Target{
			{Platform: "windows", Arch: "amd64"},
			{Platform: "linux", Arch: "arm64", Tags: []string{"linux-custom"}},
		},
	})
	require.NoError(t, err)
	require.Len(t, plan.Targets, 2)
	assert.Equal(t, []string{"production"}, plan.Targets[0].Tags)
	assert.Equal(t, []string{"production", "linux-custom"}, plan.Targets[1].Tags)

	native := findStages(plan, "native.compile")
	require.Len(t, native, 2)
	assert.Equal(t, "native.compile[windows/amd64]", native[0].Reference())
	assert.Equal(t, "bin/windows/amd64/"+filepath.Base(root)+".exe", native[0].Outputs[0].Path)
	assert.Equal(t, "binary[windows/amd64]", native[0].Outputs[0].Reference())
	assert.Equal(t, "native.compile[linux/arm64]", native[1].Reference())
	assert.Equal(t, "bin/linux/arm64/"+filepath.Base(root), native[1].Outputs[0].Path)
	assert.Equal(t, "binary[linux/arm64]", native[1].Outputs[0].Reference())

	collect := findStage(t, plan, "artifacts.collect")
	assert.Equal(t, []string{
		"native.compile[windows/amd64]",
		"native.compile[linux/arm64]",
	}, collect.Needs)
	require.Len(t, collect.Inputs, 2)
	assert.Equal(t, "binary[windows/amd64]", collect.Inputs[0].Reference())
	assert.Equal(t, "binary[linux/arm64]", collect.Inputs[1].Reference())

	_, err = InspectStage(plan, "native.compile")
	require.EqualError(
		t,
		err,
		`build stage "native.compile" is ambiguous; select one of native.compile[windows/amd64], native.compile[linux/arm64]`,
	)
	inspection, err := InspectStage(plan, "native.compile[linux/arm64]")
	require.NoError(t, err)
	assert.Equal(t, "linux", inspection.Stage.Target.Platform)
}

func TestResolveRejectsDuplicateTargets(t *testing.T) {
	_, err := Resolve(Request{
		ProjectRoot: t.TempDir(),
		Targets: []Target{
			{Platform: "linux", Arch: "amd64"},
			{Platform: "linux", Arch: "amd64"},
		},
	})
	require.EqualError(t, err, `duplicate build target "linux/amd64"`)
}

func TestResolveLoadsTargetMatrixFromConfig(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build", "config.yml"), []byte(`
build:
  targets:
    - platform: darwin
      arch: amd64
    - platform: darwin
      arch: arm64
      tags: [apple-silicon]
`), 0o644))

	plan, err := Resolve(Request{ProjectRoot: root})
	require.NoError(t, err)
	require.Len(t, plan.Targets, 2)
	assert.Equal(t, "darwin", plan.Targets[0].Platform)
	assert.Equal(t, "amd64", plan.Targets[0].Arch)
	assert.Equal(t, []string{"production"}, plan.Targets[0].Tags)
	assert.Equal(t, []string{"production", "apple-silicon"}, plan.Targets[1].Tags)
	require.Len(t, findStages(plan, "native.compile"), 2)
}

func TestResolveAppliesNativeCompileMatrixExclusions(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build", "config.yml"), []byte(`
build:
  targets:
    - platform: darwin
      arch: amd64
    - platform: darwin
      arch: arm64
    - platform: windows
      arch: amd64
  stages:
    native.compile:
      matrix:
        exclude:
          - platform: darwin
            arch: amd64
          - platform: windows
`), 0o644))

	plan, err := Resolve(Request{ProjectRoot: root})
	require.NoError(t, err)
	assert.Equal(t, []Target{{
		Platform: "darwin",
		Arch:     "arm64",
		Tags:     []string{"production"},
	}}, plan.Targets)
	require.Len(t, findStages(plan, "native.compile"), 1)
}

func TestResolveExpandsTargetPathsForMatrixReplacements(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build", "config.yml"), []byte(`
build:
  targets:
    - platform: windows
      arch: amd64
    - platform: linux
      arch: arm64
  stages:
    native.compile:
      replace:
        command: [./compile, "${target.platform}", "${target.arch}"]
        produces:
          binary: dist/${target.platform}/${target.arch}/application
`), 0o644))

	plan, err := Resolve(Request{ProjectRoot: root})
	require.NoError(t, err)
	native := findStages(plan, "native.compile")
	require.Len(t, native, 2)
	assert.Equal(t, "dist/windows/amd64/application", native[0].Outputs[0].Path)
	assert.Equal(t, "dist/linux/arm64/application", native[1].Outputs[0].Path)
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

func findStages(plan *Plan, id string) []Stage {
	var result []Stage
	for _, stage := range plan.Stages {
		if stage.ID == id {
			result = append(result, stage)
		}
	}
	return result
}
