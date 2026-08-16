package commands

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wailsapp/wails/v3/internal/buildsystem"
	"github.com/wailsapp/wails/v3/internal/flags"
	"gopkg.in/yaml.v3"
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

func TestResolveBuildActionsAppliesStageSettingsAndOverlays(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build", "config.yml"), []byte(`
build:
  frontend:
    directory: web
  stages:
    dependencies.prepare:
      settings:
        goModules: validate
        frontend: none
    frontend.build:
      settings:
        directory: ui
        output: ui/out
        build:
          command: [bun, run, bundle]
        environment:
          CHANNEL: preview
    platform.generate:
      settings:
        overlays:
          windows:
            manifest: build/overlays/application.manifest
    native.compile:
      settings:
        tags: [enterprise]
        trimPath: false
        vcsInfo: true
`), 0o644))

	plan, err := buildsystem.Resolve(buildsystem.Request{ProjectRoot: root, Target: "windows", Arch: "amd64"})
	require.NoError(t, err)
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))

	dependencies := commandStage(t, plan, "dependencies.prepare")
	require.Len(t, dependencies.Actions, 1)
	assert.Equal(t, []string{"go", "mod", "verify"}, dependencies.Actions[0].Command)

	frontend := commandStage(t, plan, "frontend.build")
	assert.Equal(t, []string{"bun", "run", "bundle"}, frontend.Actions[0].Command)
	assert.Equal(t, filepath.Join(root, "ui"), frontend.Actions[0].WorkingDirectory)
	assert.Equal(t, "preview", frontend.Actions[0].Environment["CHANNEL"])
	assert.Equal(t, "ui/out", frontend.Outputs[0].Path)

	platform := commandStage(t, plan, "platform.generate")
	require.Len(t, platform.Actions, 2)
	assert.Equal(t, filepath.Join(root, "build", "overlays", "application.manifest"), platform.Actions[1].Parameters["manifest"])

	native := commandStage(t, plan, "native.compile")
	var frontendInput string
	for _, input := range native.Inputs {
		if input.Name == "frontend" {
			frontendInput = input.Path
		}
	}
	assert.Equal(t, "ui/out", frontendInput)
	compile := native.Actions[2].Command
	assert.Contains(t, compile, "production,enterprise")
	assert.NotContains(t, compile, "-trimpath")
	assert.Contains(t, compile, "-buildvcs=true")
}

func TestResolveActionsForExplicitUserOwnedPipeline(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build", "config.yml"), []byte(`
build:
  pipeline:
    generate:
      run: [go, generate, ./...]
      produces: {generated: .wails/generated}
    assets:
      uses: wails/assets.generate
    platform:
      uses: wails/platform.generate
      needs: [assets]
    compile:
      uses: wails/native.compile
      needs: [generate, platform]
      produces: {binary: bin/custom.exe}
`), 0o644))
	plan, err := buildsystem.Resolve(buildsystem.Request{ProjectRoot: root, Target: "windows", Arch: "amd64"})
	require.NoError(t, err)
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))

	generate := commandStage(t, plan, "generate")
	require.Len(t, generate.Actions, 1)
	assert.Equal(t, []string{"go", "generate", "./..."}, generate.Actions[0].Command)

	compile := commandStage(t, plan, "compile")
	assert.Equal(t, "native.compile", compile.Operation())
	require.Len(t, compile.Actions, 4)
	assert.Contains(t, compile.Actions[2].Command, filepath.Join(root, "bin", "custom.exe"))
}

func TestResolveMinimalExplicitPipelineWithCustomPackager(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "build", "config.yml"), []byte(`
build:
  pipeline:
    generate:
      run: [go, generate, ./...]
      produces: {generated: .wails/generated}
    compile:
      uses: wails/native.compile
      needs: [generate]
      produces: {binary: bin/application}
    bundle:
      uses: wails/bundle.assemble
      needs: [compile]
      produces: {bundle: bin/Application.AppDir}
    package:
      run: [./scripts/package, "${artifacts.bundle.bundle}", bin/application.custom]
      needs: [bundle]
      produces: {package: bin/application.custom}
`), 0o644))
	plan, err := buildsystem.Resolve(buildsystem.Request{ProjectRoot: root, Target: "linux", Arch: "amd64"})
	require.NoError(t, err)
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))

	compile := commandStage(t, plan, "compile")
	assert.Equal(t, "native.compile", compile.Operation())
	bundle := commandStage(t, plan, "bundle")
	assert.Equal(t, "bundle.assemble", bundle.Operation())
	packaging := commandStage(t, plan, "package")
	assert.Equal(t, filepath.Join(root, "bin", "Application.AppDir"), packaging.Actions[0].Command[1])
}

func commandStage(t *testing.T, plan *buildsystem.Plan, id string) buildsystem.Stage {
	t.Helper()
	for _, stage := range plan.Stages {
		if stage.ID == id {
			return stage
		}
	}
	t.Fatalf("stage %q not found", id)
	return buildsystem.Stage{}
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
	require.NoError(t, json.Unmarshal([]byte(collect.Actions[0].Parameters["artifacts"]), &inputs))
	require.Len(t, inputs, 2)
	assert.Equal(t, "bundle[windows/amd64]", inputs[0].ID)
	assert.Equal(t, "bundle[linux/arm64]", inputs[1].ID)
}

func TestResolveBuildActionsExpandsUniversalDarwinStages(t *testing.T) {
	root := t.TempDir()
	plan, err := buildsystem.Resolve(buildsystem.Request{
		ProjectRoot: root,
		Targets: []buildsystem.Target{
			{Platform: "darwin", Arch: "amd64"},
			{Platform: "darwin", Arch: "arm64"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))

	combine := findBuildStage(t, plan, "binary.combine")
	require.Len(t, combine.Actions, 2)
	assert.Equal(t, buildsystem.ActionInternal, combine.Actions[1].Kind)
	assert.Equal(t, "binary.combine", combine.Actions[1].Internal)
	var inputs []string
	require.NoError(t, json.Unmarshal([]byte(combine.Actions[1].Parameters["inputs"]), &inputs))
	assert.Equal(t, []string{
		filepath.Join(root, "bin", "darwin", "amd64", filepath.Base(root)),
		filepath.Join(root, "bin", "darwin", "arm64", filepath.Base(root)),
	}, inputs)

	bundle := findBuildStage(t, plan, "bundle.assemble")
	require.Len(t, bundle.Actions, 7)
	assert.True(t, bundle.Actions[0].Recursive)
	assert.Equal(t, buildsystem.ActionCopy, bundle.Actions[3].Kind)
	assert.True(t, bundle.Actions[6].Optional)

	collect := findBuildStage(t, plan, "artifacts.collect")
	var artifacts []collectionInput
	require.NoError(t, json.Unmarshal([]byte(collect.Actions[0].Parameters["artifacts"]), &artifacts))
	require.Len(t, artifacts, 2)
	assert.Equal(t, "universal-binary", artifacts[0].Type)
	assert.Equal(t, "application-bundle", artifacts[1].Type)
}

func TestDarwinBundleActionsAssembleRunnableLayout(t *testing.T) {
	root := t.TempDir()
	plan, err := buildsystem.Resolve(buildsystem.Request{
		ProjectRoot: root,
		Target:      "darwin",
		Arch:        "arm64",
	})
	require.NoError(t, err)
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))
	bundle := findBuildStage(t, plan, "bundle.assemble")

	binary := absoluteArtifactPath(root, bundle.Inputs[0])
	platform := absoluteArtifactPath(root, bundle.Inputs[1])
	require.NoError(t, os.MkdirAll(filepath.Dir(binary), 0o755))
	require.NoError(t, os.MkdirAll(platform, 0o755))
	require.NoError(t, os.WriteFile(binary, []byte("native"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(platform, "Info.plist"), []byte("plist"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(platform, "icons.icns"), []byte("icon"), 0o644))

	require.NoError(t, buildsystem.Execute(context.Background(), plan, buildsystem.ExecuteOptions{
		Step:            bundle.Reference(),
		InternalActions: buildInternalActions(&flags.Build{}),
	}))

	bundlePath := absoluteArtifactPath(root, bundle.Outputs[0])
	bundledBinary := filepath.Join(bundlePath, "Contents", "MacOS", filepath.Base(binary))
	data, err := os.ReadFile(bundledBinary)
	require.NoError(t, err)
	assert.Equal(t, "native", string(data))
	info, err := os.Stat(bundledBinary)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o111)
	assert.FileExists(t, filepath.Join(bundlePath, "Contents", "Info.plist"))
	assert.FileExists(t, filepath.Join(bundlePath, "Contents", "Resources", "icons.icns"))
}

func TestWindowsAndLinuxBundleActionsAssembleRunnableLayouts(t *testing.T) {
	for _, target := range []buildsystem.Target{{Platform: "windows", Arch: "amd64"}, {Platform: "linux", Arch: "arm64"}} {
		t.Run(target.Platform, func(t *testing.T) {
			root := t.TempDir()
			plan, err := buildsystem.Resolve(buildsystem.Request{ProjectRoot: root, Target: target.Platform, Arch: target.Arch})
			require.NoError(t, err)
			require.NoError(t, resolveBuildActions(plan, &flags.Build{}))
			bundle := commandStage(t, plan, "bundle.assemble")
			assert.Equal(t, "planned", bundle.Status)
			require.NotEmpty(t, bundle.Actions)
			assert.Equal(t, buildsystem.ActionRemove, bundle.Actions[0].Kind)
			assert.True(t, bundle.Actions[0].Recursive)
			if target.Platform == "windows" {
				assert.Equal(t, filepath.Join(root, "bin", filepath.Base(root)+".windows"), bundle.Actions[0].Path)
				assert.Equal(t, buildsystem.ActionCopy, bundle.Actions[2].Kind)
			} else {
				assert.Equal(t, filepath.Join(root, "bin", filepath.Base(root)+".AppDir"), bundle.Actions[0].Path)
				assert.Contains(t, bundle.Actions[4].Destination, filepath.Join("usr", "bin"))
			}
		})
	}
}

func TestResolveAdditionalDesktopPackageFormats(t *testing.T) {
	tests := []struct {
		platform string
		format   string
		tool     string
	}{
		{"windows", "msix", "makeappx.exe"},
		{"linux", "appimage", "appimagetool"},
		{"darwin", "dmg", "hdiutil"},
		{"darwin", "pkg", "productbuild"},
	}
	for _, test := range tests {
		t.Run(test.platform+"-"+test.format, func(t *testing.T) {
			plan, err := buildsystem.Resolve(buildsystem.Request{
				ProjectRoot: t.TempDir(), Target: test.platform, Arch: "arm64", Goal: "package", Packages: []string{test.format},
			})
			require.NoError(t, err)
			require.NoError(t, resolveBuildActions(plan, &flags.Build{}))
			stage := commandStage(t, plan, "package.create")
			require.GreaterOrEqual(t, len(stage.Actions), 2)
			last := stage.Actions[len(stage.Actions)-1]
			assert.Equal(t, buildsystem.ActionCommand, last.Kind)
			assert.Equal(t, test.tool, last.Command[0])
		})
	}
}

func TestResolveAndroidProductionPipeline(t *testing.T) {
	root := t.TempDir()
	plan, err := buildsystem.Resolve(buildsystem.Request{
		ProjectRoot: root, Target: "android", Arch: "arm64", Goal: "package", Packages: []string{"apk", "aab"},
	})
	require.NoError(t, err)
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))

	platform := commandStage(t, plan, "platform.generate")
	assert.Equal(t, "platform.generate-android", platform.Actions[1].Internal)
	native := commandStage(t, plan, "native.compile")
	assert.Equal(t, "libwails.so", filepath.Base(native.Outputs[0].Path))
	assert.Equal(t, []string{"/bin/sh", "-c"}, native.Actions[1].Command[:2])
	assert.Contains(t, native.Actions[1].Command[2], "ANDROID_NDK_HOME")
	bundle := commandStage(t, plan, "bundle.assemble")
	assert.True(t, bundle.Actions[1].Recursive)
	assert.Contains(t, bundle.Actions[3].Destination, filepath.Join("jniLibs", "arm64-v8a", "libwails.so"))

	packages := stagesByOperation(plan, "package.create")
	require.Len(t, packages, 2)
	assert.Contains(t, packages[0].Actions[1].Command[2], "assembleRelease")
	assert.Contains(t, packages[1].Actions[1].Command[2], "bundleRelease")
}

func TestResolveIOSProductionPipeline(t *testing.T) {
	root := t.TempDir()
	plan, err := buildsystem.Resolve(buildsystem.Request{ProjectRoot: root, Target: "ios", Arch: "arm64", Goal: "package", Packages: []string{"ipa"}})
	require.NoError(t, err)
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))

	platform := commandStage(t, plan, "platform.generate")
	require.Len(t, platform.Actions, 3)
	assert.Equal(t, "platform.generate-ios-overlay", platform.Actions[1].Internal)
	assert.Equal(t, "platform.generate-ios-xcode", platform.Actions[2].Internal)
	native := commandStage(t, plan, "native.compile")
	assert.Equal(t, ".a", filepath.Ext(native.Outputs[0].Path))
	assert.Contains(t, native.Actions[1].Command[2], "xcrun --sdk iphoneos")
	bundle := commandStage(t, plan, "bundle.assemble")
	assert.Equal(t, "xcrun", bundle.Actions[2].Command[0])
	assert.Equal(t, "actool", bundle.Actions[4].Command[1])
	assert.Equal(t, []string{"codesign", "--force", "--sign", "-", filepath.Join(root, "bin", filepath.Base(root)+".app")}, bundle.Actions[len(bundle.Actions)-1].Command)
	packaging := commandStage(t, plan, "package.create")
	assert.True(t, packaging.Actions[3].Recursive)
	assert.Equal(t, "package.zip", packaging.Actions[4].Internal)
}

func TestResolveIOSDeviceSigningConfiguration(t *testing.T) {
	root := t.TempDir()
	plan, err := buildsystem.Resolve(buildsystem.Request{ProjectRoot: root, Target: "ios", Arch: "arm64", Goal: "build", Mode: "development"})
	require.NoError(t, err)
	plan.Signing.IOS.Identity = "Apple Development: Example"
	plan.Signing.IOS.Entitlements = "build/ios/entitlements.plist"
	plan.Signing.IOS.ProvisioningProfile = "build/ios/profile.mobileprovision"
	require.NoError(t, resolveBuildActions(plan, &flags.Build{}))

	bundle := commandStage(t, plan, "bundle.assemble")
	assert.Equal(t, filepath.Join(root, "build", "ios", "profile.mobileprovision"), bundle.Actions[len(bundle.Actions)-2].Source)
	assert.Equal(t, []string{
		"codesign", "--force", "--sign", "Apple Development: Example", "--entitlements",
		filepath.Join(root, "build", "ios", "entitlements.plist"), filepath.Join(root, "bin", filepath.Base(root)+".app"),
	}, bundle.Actions[len(bundle.Actions)-1].Command)
}

func stagesByOperation(plan *buildsystem.Plan, operation string) []buildsystem.Stage {
	var result []buildsystem.Stage
	for _, stage := range plan.Stages {
		if stage.Operation() == operation && stage.Status == "planned" {
			result = append(result, stage)
		}
	}
	return result
}

func TestCollectArtifactsWritesFilesAndBundles(t *testing.T) {
	root := t.TempDir()
	windowsPath := filepath.Join(root, "bin", "windows", "amd64", "app.exe")
	linuxPath := filepath.Join(root, "bin", "linux", "arm64", "app")
	require.NoError(t, os.MkdirAll(filepath.Dir(windowsPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(linuxPath), 0o755))
	require.NoError(t, os.WriteFile(windowsPath, []byte("windows"), 0o755))
	require.NoError(t, os.WriteFile(linuxPath, []byte("linux"), 0o755))
	bundlePath := filepath.Join(root, "bin", "darwin", "universal", "App.app")
	require.NoError(t, os.MkdirAll(filepath.Join(bundlePath, "Contents", "MacOS"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bundlePath, "Contents", "MacOS", "app"), []byte("darwin"), 0o755))
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
			ID:         "bundle[darwin/universal]",
			Type:       "application-bundle",
			Path:       bundlePath,
			ReportPath: "bin/darwin/universal/App.app",
			Platform:   "darwin",
			Arch:       "universal",
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
			inputs[1].ID: {ID: inputs[1].ID, Name: "bundle"},
			inputs[2].ID: {ID: inputs[2].ID, Name: "binary"},
		},
	}
	require.NoError(t, collectArtifactsAction(context.Background(), stage, buildsystem.Action{
		Parameters: map[string]string{
			"artifacts": string(encoded),
			"manifest":  manifestPath,
		},
	}))

	data, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	var manifest buildArtifactManifest
	require.NoError(t, json.Unmarshal(data, &manifest))
	require.Len(t, manifest.Artifacts, 3)
	assert.Equal(t, "windows", manifest.Artifacts[0].Platform)
	assert.Equal(t, "binary[windows/amd64]", manifest.Artifacts[0].ID)
	assert.Equal(t, "bin/windows/amd64/app.exe", manifest.Artifacts[0].Path)
	assert.NotEmpty(t, manifest.Artifacts[0].SHA256)
	assert.Equal(t, "application-bundle", manifest.Artifacts[1].Type)
	assert.Equal(t, "bundle[darwin/universal]", manifest.Artifacts[1].ID)
	assert.Equal(t, int64(len("darwin")), manifest.Artifacts[1].Size)
	assert.NotEmpty(t, manifest.Artifacts[1].SHA256)
	assert.Equal(t, "linux", manifest.Artifacts[2].Platform)
}

func TestResolveTypedPackageActions(t *testing.T) {
	tests := []struct {
		name       string
		platform   string
		format     string
		actionKind buildsystem.ActionKind
		operation  string
	}{
		{name: "macOS zip", platform: "darwin", format: "zip", actionKind: buildsystem.ActionInternal, operation: "package.zip"},
		{name: "Windows NSIS", platform: "windows", format: "nsis", actionKind: buildsystem.ActionCommand},
		{name: "Linux deb", platform: "linux", format: "deb", actionKind: buildsystem.ActionInternal, operation: "package.linux"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := buildsystem.Resolve(buildsystem.Request{
				ProjectRoot: t.TempDir(),
				Target:      test.platform,
				Arch:        "amd64",
				Goal:        "package",
				Packages:    []string{test.format},
			})
			require.NoError(t, err)
			require.NoError(t, resolveTypedPipelineActions(plan, &flags.Build{}, nil))
			stage := findBuildStage(t, plan, "package.create")
			require.GreaterOrEqual(t, len(stage.Actions), 2)
			assert.Equal(t, test.actionKind, stage.Actions[1].Kind)
			assert.Equal(t, test.operation, stage.Actions[1].Internal)
			assert.Equal(t, test.format, stage.Outputs[0].Target.Format)
		})
	}
}

func TestResolveTypedSigningAndNotarizationActions(t *testing.T) {
	plan, err := buildsystem.Resolve(buildsystem.Request{
		ProjectRoot: t.TempDir(),
		Target:      "darwin",
		Arch:        "arm64",
		Goal:        "notarize",
	})
	require.NoError(t, err)
	signing := &flags.Sign{
		Identity:        "Developer ID Application: Example",
		HardenedRuntime: true,
		KeychainProfile: "release-notary",
	}
	require.NoError(t, resolveTypedPipelineActions(plan, &flags.Build{}, signing))

	bundleSign := findBuildStage(t, plan, "bundle.sign")
	require.Len(t, bundleSign.Actions, 1)
	assert.Equal(t, "artifact.sign", bundleSign.Actions[0].Internal)
	assert.Equal(t, signing.Identity, bundleSign.Actions[0].Parameters["identity"])
	assert.Equal(t, "true", bundleSign.Actions[0].Parameters["hardenedRuntime"])

	notarize := findBuildStage(t, plan, "package.notarize")
	require.Len(t, notarize.Actions, 2)
	assert.Equal(t, "artifact.notarize", notarize.Actions[0].Internal)
	assert.Equal(t, "release-notary", notarize.Actions[0].Parameters["keychainProfile"])
	assert.Equal(t, "package.zip", notarize.Actions[1].Internal)
}

func TestResolvePipelineSigningOptionsUsesProjectConfiguration(t *testing.T) {
	root := t.TempDir()
	plan := &buildsystem.Plan{Project: buildsystem.Project{Root: root}, Goal: "notarize"}
	plan.Signing.Darwin.Identity = "Developer ID Application: Project"
	plan.Signing.Darwin.Entitlements = "build/darwin/entitlements.plist"
	plan.Signing.Darwin.KeychainProfile = "project-notary"
	plan.Signing.Windows.Certificate = "build/windows/release.pfx"
	plan.Signing.Linux.PGPKey = "build/linux/release.asc"

	resolved := resolvePipelineSigningOptions(plan, &flags.Sign{Identity: "CLI Identity"})
	assert.Equal(t, "CLI Identity", resolved.Identity)
	assert.Equal(t, filepath.Join(root, "build", "darwin", "entitlements.plist"), resolved.Entitlements)
	assert.Equal(t, filepath.Join(root, "build", "windows", "release.pfx"), resolved.Certificate)
	assert.Equal(t, filepath.Join(root, "build", "linux", "release.asc"), resolved.PGPKey)
	assert.Equal(t, "project-notary", resolved.KeychainProfile)
	assert.True(t, resolved.HardenedRuntime)
	assert.False(t, resolved.Notarize)
}

func TestResolvePipelineSigningOptionsUsesEnvironmentProvider(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RELEASE_IDENTITY", "Environment Identity")
	t.Setenv("RELEASE_CERTIFICATE", "certificates/release.pfx")
	t.Setenv("RELEASE_KEYCHAIN_PROFILE", "environment-notary")
	plan := &buildsystem.Plan{Project: buildsystem.Project{Root: root}}
	plan.Signing.Credentials.Provider = "environment"
	plan.Signing.Credentials.Prefix = "RELEASE_"

	resolved := resolvePipelineSigningOptions(plan, nil)
	assert.Equal(t, "Environment Identity", resolved.Identity)
	assert.Equal(t, filepath.Join(root, "certificates", "release.pfx"), resolved.Certificate)
	assert.Equal(t, "environment-notary", resolved.KeychainProfile)
}

func TestCreateZipArtifactIsDeterministic(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "Example.app")
	executable := filepath.Join(bundle, "Contents", "MacOS", "example")
	require.NoError(t, os.MkdirAll(filepath.Dir(executable), 0o755))
	require.NoError(t, os.WriteFile(executable, []byte("native"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte("plist"), 0o644))

	first := filepath.Join(root, "first.zip")
	second := filepath.Join(root, "second.zip")
	require.NoError(t, createZipArtifact(context.Background(), bundle, first))
	require.NoError(t, createZipArtifact(context.Background(), bundle, second))
	firstData, err := os.ReadFile(first)
	require.NoError(t, err)
	secondData, err := os.ReadFile(second)
	require.NoError(t, err)
	assert.Equal(t, firstData, secondData)

	archive, err := zip.OpenReader(first)
	require.NoError(t, err)
	defer archive.Close()
	var archivedExecutable *zip.File
	for _, file := range archive.File {
		if file.Name == "Example.app/Contents/MacOS/example" {
			archivedExecutable = file
			break
		}
	}
	require.NotNil(t, archivedExecutable)
	if runtime.GOOS != "windows" {
		assert.NotZero(t, archivedExecutable.Mode().Perm()&0o111)
	}
}

func TestRewritePackageConfigResolvesMatrixInput(t *testing.T) {
	var document yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(`
arch: ${GOARCH}
contents:
  - src: ./bin/example
    dst: /usr/local/bin/example
`), &document))
	rewritePackageConfig(&document, "example", "/workspace/bin/linux/arm64/example", "arm64")
	data, err := yaml.Marshal(&document)
	require.NoError(t, err)
	assert.Contains(t, string(data), "arch: arm64")
	assert.Contains(t, string(data), "src: /workspace/bin/linux/arm64/example")
}

func findBuildStage(t *testing.T, plan *buildsystem.Plan, id string) buildsystem.Stage {
	t.Helper()
	for _, stage := range plan.Stages {
		if stage.ID == id && stage.Status == "planned" {
			return stage
		}
	}
	t.Fatalf("planned stage %q not found", id)
	return buildsystem.Stage{}
}
