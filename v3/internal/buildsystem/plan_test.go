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
	assert.Equal(t, "target uses the native binary as its runnable artifact", bundle.Reason)

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

func TestResolvePlansDarwinApplicationBundle(t *testing.T) {
	root := t.TempDir()
	plan, err := Resolve(Request{ProjectRoot: root, Target: "darwin", Arch: "arm64"})
	require.NoError(t, err)

	combine := findStage(t, plan, "binary.combine")
	assert.Equal(t, "skipped", combine.Status)
	assert.Equal(t, "single-architecture build", combine.Reason)

	bundle := findStage(t, plan, "bundle.assemble")
	assert.Equal(t, "planned", bundle.Status)
	assert.Equal(t, "bundle.assemble[darwin/arm64]", bundle.Reference())
	require.Len(t, bundle.Outputs, 1)
	assert.Equal(t, "bundle[darwin/arm64]", bundle.Outputs[0].Reference())
	assert.Equal(t, filepath.ToSlash(filepath.Join("bin", filepath.Base(root)+".app")), bundle.Outputs[0].Path)

	collect := findStage(t, plan, "artifacts.collect")
	assert.Equal(t, []string{"native.compile[darwin/arm64]", bundle.Reference()}, collect.Needs)
	require.Len(t, collect.Inputs, 2)
	assert.Equal(t, "binary[darwin/arm64]", collect.Inputs[0].Reference())
	assert.Equal(t, "bundle[darwin/arm64]", collect.Inputs[1].Reference())
}

func TestResolvePlansUniversalDarwinBinaryAndBundle(t *testing.T) {
	root := t.TempDir()
	plan, err := Resolve(Request{
		ProjectRoot: root,
		Targets: []Target{
			{Platform: "darwin", Arch: "amd64"},
			{Platform: "darwin", Arch: "arm64"},
		},
	})
	require.NoError(t, err)

	combines := findStages(plan, "binary.combine")
	require.Len(t, combines, 1)
	combine := combines[0]
	assert.Equal(t, "planned", combine.Status)
	assert.Equal(t, "binary.combine[darwin/universal]", combine.Reference())
	assert.Equal(t, []string{"production"}, combine.Target.Tags)
	assert.Equal(t, []string{
		"native.compile[darwin/amd64]",
		"native.compile[darwin/arm64]",
	}, combine.Needs)
	require.Len(t, combine.Inputs, 2)
	require.Len(t, combine.Outputs, 1)
	assert.Equal(t, "binary[darwin/universal]", combine.Outputs[0].Reference())
	assert.Equal(t, filepath.ToSlash(filepath.Join("bin", "darwin", "universal", filepath.Base(root))), combine.Outputs[0].Path)

	bundles := findStages(plan, "bundle.assemble")
	require.Len(t, bundles, 1)
	bundle := bundles[0]
	assert.Equal(t, "bundle.assemble[darwin/universal]", bundle.Reference())
	assert.Equal(t, "bundle[darwin/universal]", bundle.Outputs[0].Reference())
	assert.Equal(t, filepath.ToSlash(filepath.Join("bin", "darwin", "universal", filepath.Base(root)+".app")), bundle.Outputs[0].Path)

	collect := findStage(t, plan, "artifacts.collect")
	assert.Equal(t, []string{combine.Reference(), bundle.Reference()}, collect.Needs)
	require.Len(t, collect.Inputs, 2)
	assert.Equal(t, "binary[darwin/universal]", collect.Inputs[0].Reference())
	assert.Equal(t, "bundle[darwin/universal]", collect.Inputs[1].Reference())
}

func TestResolvePackageGoalFansOutLinuxFormats(t *testing.T) {
	plan, err := Resolve(Request{
		ProjectRoot: t.TempDir(),
		Target:      "linux",
		Arch:        "amd64",
		Goal:        "package",
	})
	require.NoError(t, err)

	packages := findStages(plan, "package.create")
	require.Len(t, packages, 3)
	assert.Equal(t, "package.create[linux/amd64/deb]", packages[0].Reference())
	assert.Equal(t, "package[linux/amd64/deb]", packages[0].Outputs[0].Reference())
	assert.Equal(t, "package.create[linux/amd64/rpm]", packages[1].Reference())
	assert.Equal(t, "package.create[linux/amd64/archlinux]", packages[2].Reference())
	for _, stage := range packages {
		assert.Equal(t, "planned", stage.Status)
	}

	collect := findStage(t, plan, "artifacts.collect")
	require.Len(t, collect.Inputs, 3)
	assert.Equal(t, []string{
		"package.create[linux/amd64/deb]",
		"package.create[linux/amd64/rpm]",
		"package.create[linux/amd64/archlinux]",
	}, collect.Needs)
}

func TestResolveSignGoalActivatesWindowsSigning(t *testing.T) {
	plan, err := Resolve(Request{
		ProjectRoot: t.TempDir(),
		Target:      "windows",
		Arch:        "amd64",
		Goal:        "sign",
	})
	require.NoError(t, err)

	bundleSign := findStage(t, plan, "bundle.sign")
	assert.Equal(t, "planned", bundleSign.Status)
	assert.Equal(t, "binary[windows/amd64]", bundleSign.Inputs[0].Reference())
	packageSign := findStage(t, plan, "package.sign")
	assert.Equal(t, "planned", packageSign.Status)
	assert.Equal(t, "package.sign[windows/amd64/nsis]", packageSign.Reference())
	assert.Equal(t, "package[windows/amd64/nsis]", packageSign.Inputs[0].Reference())
}

func TestResolveNotarizeGoalActivatesDarwinNotarization(t *testing.T) {
	plan, err := Resolve(Request{
		ProjectRoot: t.TempDir(),
		Target:      "darwin",
		Arch:        "arm64",
		Goal:        "notarize",
	})
	require.NoError(t, err)

	assert.Equal(t, "planned", findStage(t, plan, "bundle.sign").Status)
	notarize := findStage(t, plan, "package.notarize")
	assert.Equal(t, "planned", notarize.Status)
	assert.Equal(t, "package.notarize[darwin/arm64/zip]", notarize.Reference())
	require.Len(t, notarize.Inputs, 2)
	assert.Equal(t, "bundle[darwin/arm64]", notarize.Inputs[0].Reference())
	assert.Equal(t, "package[darwin/arm64/zip]", notarize.Inputs[1].Reference())
}

func TestResolveRejectsInvalidGoalAndPackageFormats(t *testing.T) {
	_, err := Resolve(Request{ProjectRoot: t.TempDir(), Goal: "deploy"})
	require.EqualError(t, err, `unsupported build goal "deploy"`)

	_, err = Resolve(Request{
		ProjectRoot: t.TempDir(),
		Goal:        "package",
		Packages:    []string{"zip", "ZIP"},
	})
	require.EqualError(t, err, `duplicate package format "zip"`)
}

func TestResolveLoadsPackageAndSigningConfiguration(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build", "config.yml"), []byte(`
build:
  packages:
    - format: zip
  signing:
    darwin:
      identity: "Developer ID Application: Example"
      entitlements: build/darwin/entitlements.plist
      keychainProfile: release-notary
    windows:
      certificate: build/windows/release.pfx
      timestampServer: https://timestamp.example.com
    linux:
      pgpKey: build/linux/release.asc
      role: builder
`), 0o644))

	plan, err := Resolve(Request{
		ProjectRoot: root,
		Target:      "darwin",
		Arch:        "arm64",
		Goal:        "package",
	})
	require.NoError(t, err)
	packages := findStages(plan, "package.create")
	require.Len(t, packages, 1)
	assert.Equal(t, "zip", packages[0].Outputs[0].Target.Format)
	assert.Equal(t, "Developer ID Application: Example", plan.Signing.Darwin.Identity)
	assert.Equal(t, "build/windows/release.pfx", plan.Signing.Windows.Certificate)
	assert.Equal(t, "builder", plan.Signing.Linux.Role)
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

func TestResolveStageExtensionContractPerMatrixInstance(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build", "config.yml"), []byte(`
build:
  targets:
    - {platform: darwin, arch: arm64}
    - {platform: linux, arch: amd64}
  stages:
    native.compile:
      settings:
        tags: [enterprise]
        trimPath: false
        vcsInfo: true
      before:
        - name: prepare-native
          command: [./scripts/prepare]
          scope: target
          shell: true
          when:
            platform: darwin
            mode: production
          inputs:
            source: go.mod
          outputs:
            prepared: .wails/build/${target.platform}/${target.arch}/prepared
          sources: [scripts/prepare]
          cache:
            files: [go.mod]
            environment: [CC]
            values: {generator: v1}
          onFailure: continue
          timeout: 20s
`), 0o644))

	plan, err := Resolve(Request{ProjectRoot: root})
	require.NoError(t, err)
	native := findStages(plan, "native.compile")
	require.Len(t, native, 2)

	assert.Equal(t, map[string]any{
		"tags": []any{"enterprise"}, "trimPath": false, "vcsInfo": true,
	}, native[0].Settings)
	require.Len(t, native[0].Before, 1)
	darwinHook := native[0].Before[0]
	assert.Equal(t, "planned", darwinHook.Status)
	assert.Equal(t, "target", darwinHook.Scope)
	assert.True(t, darwinHook.Shell)
	assert.NotEmpty(t, darwinHook.ResolvedCommand)
	assert.Contains(t, darwinHook.ResolvedCommand[len(darwinHook.ResolvedCommand)-1], "./scripts/prepare")
	assert.Equal(t, "continue", darwinHook.OnFailure)
	assert.Equal(t, []string{"go.mod", "scripts/prepare"}, darwinHook.Cache.Files)
	assert.Equal(t, []string{"CC"}, darwinHook.Cache.Environment)
	assert.Equal(t, "v1", darwinHook.Cache.Values["generator"])
	assert.Equal(t, ".wails/build/darwin/arm64/prepared", native[0].Outputs[len(native[0].Outputs)-1].Path)

	require.Len(t, native[1].Before, 1)
	assert.Equal(t, "skipped", native[1].Before[0].Status)
	assert.Contains(t, native[1].Before[0].Reason, "platform")
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
