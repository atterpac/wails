package commands

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/shlex"
	"github.com/wailsapp/wails/v3/internal/buildsystem"
	"github.com/wailsapp/wails/v3/internal/flags"
	"github.com/wailsapp/wails/v3/internal/packager"
	"github.com/wailsapp/wails/v3/internal/term"
	"gopkg.in/yaml.v3"
)

func executeBuildPipeline(buildFlags *flags.Build, otherArgs []string, step string) error {
	return executeTypedPipeline(buildFlags, nil, "build", otherArgs, step)
}

func executeTypedPipeline(
	buildFlags *flags.Build,
	signOptions *flags.Sign,
	goal string,
	otherArgs []string,
	step string,
) error {
	target, arch := targetFromArgs(otherArgs)
	mode := buildModeFromArgs(otherArgs)
	targets, err := requestedTargets(buildFlags.Targets)
	if err != nil {
		return err
	}
	plan, err := buildsystem.Resolve(buildsystem.Request{
		ConfigPath: buildFlags.Config,
		Targets:    targets,
		Target:     target,
		Arch:       arch,
		Mode:       mode,
		Tags:       strings.Split(buildFlags.Tags, ","),
		Obfuscated: buildFlags.Obfuscated,
		Server:     buildFlags.Server,
		Goal:       goal,
		Packages:   buildFlags.Packages,
	})
	if err != nil {
		return err
	}
	if err := resolveTypedPipelineActions(plan, buildFlags, signOptions); err != nil {
		return err
	}

	term.Header("Build Pipeline")
	DisableFooter = true
	return buildsystem.Execute(context.Background(), plan, buildsystem.ExecuteOptions{
		From:            buildFlags.From,
		Until:           buildFlags.Until,
		Step:            step,
		Parallel:        buildFlags.Parallel,
		DisableCache:    buildFlags.NoCache,
		Resume:          buildFlags.Resume,
		CacheDirectory:  buildFlags.CacheDir,
		ReportPath:      buildFlags.Report,
		Stdout:          os.Stdout,
		Stderr:          os.Stderr,
		InternalActions: buildInternalActions(buildFlags),
		OnStage: func(stage buildsystem.Stage, status string) {
			switch status {
			case "running":
				term.Infof("%s", stage.Reference())
			case "completed":
				term.Success(stage.Reference())
			case "skipped":
				term.Infof("%s (skipped: %s)", stage.Reference(), stage.Reason)
			case "cached":
				term.Success(fmt.Sprintf("%s (cache hit)", stage.Reference()))
			case "resumed":
				term.Success(fmt.Sprintf("%s (resumed)", stage.Reference()))
			}
		},
	})
}

func resolveBuildActions(plan *buildsystem.Plan, buildFlags *flags.Build) error {
	return resolveTypedPipelineActions(plan, buildFlags, nil)
}

func resolveTypedPipelineActions(
	plan *buildsystem.Plan,
	buildFlags *flags.Build,
	signOptions *flags.Sign,
) error {
	signOptions = resolvePipelineSigningOptions(plan, signOptions)
	for index := range plan.Stages {
		stage := &plan.Stages[index]
		if stage.Status == "skipped" || stage.Replacement != nil {
			continue
		}

		switch stage.Operation() {
		case "project.resolve":
			stage.Actions = []buildsystem.Action{{
				Kind:        buildsystem.ActionMkdir,
				Description: "Create the generated build workspace",
				Status:      "planned",
				Path:        filepath.Join(plan.Project.Root, ".wails", "build"),
			}}
		case "toolchain.check":
			stage.Actions = toolchainActions(plan, *stage)
		case "dependencies.prepare":
			stage.Actions = dependencyActions(plan, *stage)
		case "bindings.generate":
			output, err := planStageOutputPath(plan, *stage, "bindings")
			if err != nil {
				return err
			}
			stage.Actions = []buildsystem.Action{internalBuildActionWithParameters(
				"bindings.generate",
				"Generate frontend bindings from the application Go packages",
				map[string]string{
					"buildFlags": bindingBuildFlags(plan),
					"clean":      "true",
					"index":      "index",
					"models":     "models",
					"obfuscated": fmt.Sprint(buildFlags.Obfuscated),
					"output":     output,
					"timeType":   "Date",
					"typescript": fmt.Sprint(fileExists(filepath.Join(
						plan.Project.Root,
						plan.Project.Frontend,
						"tsconfig.json",
					))),
				},
			)}
		case "assets.generate":
			actions, err := assetActions(plan, *stage)
			if err != nil {
				return err
			}
			stage.Actions = actions
		case "frontend.build":
			directory := filepath.Join(plan.Project.Root, plan.Project.Frontend)
			if configured, ok := stageSettingString(*stage, "directory"); ok {
				directory = resolveProjectPath(plan, configured)
			}
			packageManager := plan.Project.PackageManager
			if configured, ok := stageSettingString(*stage, "packageManager"); ok {
				packageManager = configured
			}
			command := []string{packageManager, "run", "build"}
			description := "Build the production frontend distribution"
			environment := map[string]string{"PRODUCTION": "true"}
			if plan.Mode == "development" {
				command = []string{packageManager, "run", "build:dev"}
				description = "Build the development frontend distribution"
				environment["PRODUCTION"] = "false"
			}
			if configured, ok := stageSettingCommand(*stage, "build"); ok {
				command = configured
			}
			if configured, ok := stageSettingMap(*stage, "environment"); ok {
				for name, value := range configured {
					environment[name] = fmt.Sprint(value)
				}
			}
			var actions []buildsystem.Action
			if install, ok := stageSettingCommand(*stage, "install"); ok {
				actions = append(actions, buildsystem.Action{
					Kind: buildsystem.ActionCommand, Description: "Install frontend dependencies", Status: "planned",
					Command: install, WorkingDirectory: directory,
				})
			}
			stage.Actions = append(actions, buildsystem.Action{
				Kind:             buildsystem.ActionCommand,
				Description:      description,
				Status:           "planned",
				Command:          command,
				WorkingDirectory: directory,
				Environment:      environment,
			})
		case "platform.generate":
			actions, err := platformActions(plan, *stage)
			if err != nil {
				return err
			}
			stage.Actions = actions
		case "native.compile":
			actions, err := nativeCompileActions(plan, *stage, buildFlags)
			if err != nil {
				return err
			}
			stage.Actions = actions
		case "binary.combine":
			actions, err := binaryCombineActions(plan, *stage)
			if err != nil {
				return err
			}
			stage.Actions = actions
		case "bundle.assemble":
			actions, err := bundleAssembleActions(plan, *stage)
			if err != nil {
				return err
			}
			stage.Actions = actions
		case "bundle.sign", "package.sign":
			actions, err := signingActions(plan, *stage, signOptions)
			if err != nil {
				return err
			}
			stage.Actions = actions
		case "package.create":
			actions, err := packageCreateActions(plan, *stage)
			if err != nil {
				return err
			}
			stage.Actions = actions
		case "package.notarize":
			actions, err := notarizeActions(plan, *stage, signOptions)
			if err != nil {
				return err
			}
			stage.Actions = actions
		case "artifacts.collect":
			manifest, err := planStageOutputPath(plan, *stage, "manifest")
			if err != nil {
				return err
			}
			artifacts, err := json.Marshal(collectionInputs(plan, stage.Inputs))
			if err != nil {
				return fmt.Errorf("encode artifact collection inputs: %w", err)
			}
			stage.Actions = []buildsystem.Action{internalBuildActionWithParameters(
				"artifacts.collect",
				"Hash final build artifacts and write the artifact manifest",
				map[string]string{
					"artifacts": string(artifacts),
					"manifest":  manifest,
				},
			)}
		}
	}
	buildsystem.ResolveExpressions(plan)
	addActionContextEnvironment(plan)
	return nil
}

func bindingBuildFlags(plan *buildsystem.Plan) string {
	tags := planTags(plan)
	if len(tags) == 0 {
		return ""
	}
	return "-tags " + strings.Join(tags, ",")
}

func toolchainActions(plan *buildsystem.Plan, stage buildsystem.Stage) []buildsystem.Action {
	target := stage.Target
	if target == nil {
		return nil
	}
	tools := []string{"go"}
	if fileExists(filepath.Join(plan.Project.Root, plan.Project.Frontend, "package.json")) {
		tools = append(tools, plan.Project.PackageManager)
	}
	if target.Platform == "linux" || target.Platform == "darwin" {
		tools = append(tools, "cc")
	}
	if target.Platform == "android" {
		tools = append(tools, "java")
	}
	if target.Platform == "ios" {
		tools = append(tools, "xcrun")
	}
	for _, candidate := range plan.Stages {
		if candidate.Status != "planned" || candidate.Target == nil ||
			candidate.Target.Platform != target.Platform ||
			(candidate.Target.Arch != target.Arch && candidate.Target.Arch != "universal") {
			continue
		}
		switch candidate.Operation() {
		case "package.create":
			if len(candidate.Outputs) > 0 {
				switch candidate.Outputs[0].Target.Format {
				case "nsis":
					tools = appendUnique(tools, "makensis")
				case "msix":
					tools = appendUnique(tools, "makeappx.exe")
				case "appimage":
					tools = appendUnique(tools, "appimagetool")
				case "dmg":
					tools = appendUnique(tools, "hdiutil")
				case "pkg":
					tools = appendUnique(tools, "productbuild")
				}
			}
		case "bundle.sign":
			switch target.Platform {
			case "darwin":
				tools = appendUnique(tools, "codesign")
			case "windows":
				tools = appendUnique(tools, "signtool.exe")
			}
		case "package.sign":
			format := candidate.Inputs[0].Target.Format
			switch format {
			case "deb":
				tools = appendUnique(tools, "dpkg-sig")
			case "rpm":
				tools = appendUnique(tools, "rpmsign")
			}
		case "package.notarize":
			tools = appendUnique(tools, "ditto")
			tools = appendUnique(tools, "xcrun")
		}
	}
	actions := make([]buildsystem.Action, 0, len(tools)+1)
	for _, tool := range tools {
		actions = append(actions, buildsystem.Action{
			Kind:        buildsystem.ActionCheckTool,
			Description: "Check that " + tool + " is available",
			Status:      "planned",
			Tool:        tool,
		})
	}
	if target.Platform == "linux" && runtime.GOOS == "linux" {
		actions = append(actions, buildsystem.Action{
			Kind:             buildsystem.ActionCommand,
			Description:      "Check Linux GUI development packages",
			Status:           "planned",
			Command:          []string{"pkg-config", "--exists", "gtk4", "webkitgtk-6.0"},
			WorkingDirectory: plan.Project.Root,
		})
	}
	return actions
}

func appendUnique(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func appendUniqueStrings(values []string, additions ...string) []string {
	result := slices.Clone(values)
	for _, value := range additions {
		result = appendUnique(result, value)
	}
	return result
}

func stageSettingMap(stage buildsystem.Stage, name string) (map[string]any, bool) {
	value, ok := stage.Settings[name]
	if !ok {
		return nil, false
	}
	result, ok := value.(map[string]any)
	return result, ok
}

func stageSettingString(stage buildsystem.Stage, name string) (string, bool) {
	value, ok := stage.Settings[name]
	result, valid := value.(string)
	return result, ok && valid
}

func stageSettingStrings(stage buildsystem.Stage, name string) ([]string, bool) {
	value, ok := stage.Settings[name]
	if !ok {
		return nil, false
	}
	switch values := value.(type) {
	case []string:
		return values, true
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			text, valid := value.(string)
			if !valid {
				return nil, false
			}
			result = append(result, text)
		}
		return result, true
	default:
		return nil, false
	}
}

func stageSettingBoolDefault(stage buildsystem.Stage, name string, fallback bool) bool {
	value, ok := stage.Settings[name].(bool)
	if !ok {
		return fallback
	}
	return value
}

func stageSettingCommand(stage buildsystem.Stage, name string) ([]string, bool) {
	group, ok := stageSettingMap(stage, name)
	if !ok {
		return nil, false
	}
	value, ok := group["command"]
	if !ok {
		return nil, false
	}
	temporary := stage
	temporary.Settings = map[string]any{"command": value}
	return stageSettingStrings(temporary, "command")
}

func stageOverlay(stage buildsystem.Stage, platform, name string) (string, bool) {
	overlays, ok := stageSettingMap(stage, "overlays")
	if !ok {
		return "", false
	}
	platformSettings, ok := overlays[platform].(map[string]any)
	if !ok {
		return "", false
	}
	value, ok := platformSettings[name].(string)
	return value, ok
}

func resolveProjectPath(plan *buildsystem.Plan, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(plan.Project.Root, filepath.FromSlash(path))
}

func assetActions(plan *buildsystem.Plan, stage buildsystem.Stage) ([]buildsystem.Action, error) {
	if stage.Target == nil {
		return nil, fmt.Errorf("stage %s has no target", stage.Reference())
	}
	output, err := planStageOutputPath(plan, stage, "assets")
	if err != nil {
		return nil, err
	}
	input := filepath.Join(plan.Project.Root, "build", "appicon.png")
	actions := []buildsystem.Action{{
		Kind:        buildsystem.ActionMkdir,
		Description: "Create the generated asset directory",
		Status:      "planned",
		Path:        output,
	}}
	switch stage.Target.Platform {
	case "windows":
		return append(actions, internalBuildActionWithParameters(
			"assets.generate-icons",
			"Encode the application icon as a Windows icon",
			map[string]string{
				"input":  input,
				"target": "windows",
				"output": filepath.Join(output, "icon.ico"),
				"sizes":  "256,128,64,48,32,16",
			},
		)), nil
	case "darwin":
		return append(actions, internalBuildActionWithParameters(
			"assets.generate-icons",
			"Encode the application icon as a macOS icon bundle",
			map[string]string{
				"input":  input,
				"target": "darwin",
				"output": filepath.Join(output, "icons.icns"),
			},
		)), nil
	default:
		return append(actions, buildsystem.Action{
			Kind:        buildsystem.ActionCopy,
			Description: "Copy the application icon into generated assets",
			Status:      "planned",
			Source:      input,
			Destination: filepath.Join(output, "appicon.png"),
		}), nil
	}
}

func platformActions(plan *buildsystem.Plan, stage buildsystem.Stage) ([]buildsystem.Action, error) {
	if stage.Target == nil {
		return nil, fmt.Errorf("stage %s has no target", stage.Reference())
	}
	output, err := planStageOutputPath(plan, stage, "platform")
	if err != nil {
		return nil, err
	}
	actions := []buildsystem.Action{{
		Kind:        buildsystem.ActionMkdir,
		Description: "Create the generated platform resource directory",
		Status:      "planned",
		Path:        output,
	}}
	switch stage.Target.Platform {
	case "windows":
		assets, err := stageInputPath(plan, stage, "assets")
		if err != nil {
			return nil, err
		}
		manifest := filepath.Join(plan.Project.Root, "build", "windows", "wails.exe.manifest")
		if configured, ok := stageOverlay(stage, "windows", "manifest"); ok {
			manifest = resolveProjectPath(plan, configured)
		}
		return append(actions, internalBuildActionWithParameters(
			"platform.generate-syso",
			"Generate the Windows resource object",
			map[string]string{
				"arch":     stage.Target.Arch,
				"icon":     filepath.Join(assets, "icon.ico"),
				"info":     filepath.Join(plan.Project.Root, "build", "windows", "info.json"),
				"manifest": manifest,
				"output":   filepath.Join(output, "rsrc_windows_"+stage.Target.Arch+".syso"),
			},
		)), nil
	case "darwin":
		assets, err := stageInputPath(plan, stage, "assets")
		if err != nil {
			return nil, err
		}
		plistName := "Info.plist"
		if plan.Mode == "development" {
			plistName = "Info.dev.plist"
		}
		plist := filepath.Join(plan.Project.Root, "build", "darwin", plistName)
		if configured, ok := stageOverlay(stage, "darwin", "plist"); ok {
			plist = resolveProjectPath(plan, configured)
		}
		return append(actions,
			buildsystem.Action{
				Kind:        buildsystem.ActionCopy,
				Description: "Copy the macOS application property list",
				Status:      "planned",
				Source:      plist,
				Destination: filepath.Join(output, "Info.plist"),
			},
			buildsystem.Action{
				Kind:        buildsystem.ActionCopy,
				Description: "Copy the macOS application icon bundle",
				Status:      "planned",
				Source:      filepath.Join(assets, "icons.icns"),
				Destination: filepath.Join(output, "icons.icns"),
			},
		), nil
	case "android":
		return append(actions, internalBuildActionWithParameters(
			"platform.generate-android",
			"Generate the Android Go overlay",
			map[string]string{
				"config": plan.Project.Config,
				"output": filepath.Join(output, "overlay.json"),
			},
		)), nil
	case "ios":
		return append(actions,
			internalBuildActionWithParameters(
				"platform.generate-ios-overlay",
				"Generate the iOS Go overlay",
				map[string]string{"config": plan.Project.Config, "output": filepath.Join(output, "overlay.json")},
			),
			internalBuildActionWithParameters(
				"platform.generate-ios-xcode",
				"Generate the iOS Xcode project and assets",
				map[string]string{"config": plan.Project.Config, "output": output},
			),
		), nil
	default:
		return actions, nil
	}
}

func addActionContextEnvironment(plan *buildsystem.Plan) {
	for stageIndex := range plan.Stages {
		stage := &plan.Stages[stageIndex]
		contextPath := filepath.Join(
			plan.Project.Root,
			".wails",
			"build",
			"context",
			buildsystem.SafeInstanceName(stage.Reference())+".json",
		)
		for actionIndex := range stage.Actions {
			action := &stage.Actions[actionIndex]
			if action.Kind != buildsystem.ActionCommand {
				continue
			}
			if action.Environment == nil {
				action.Environment = make(map[string]string)
			}
			action.Environment["WAILS_BUILD_STAGE"] = stage.Reference()
			action.Environment["WAILS_BUILD_CONTEXT"] = contextPath
		}
	}
}

func dependencyActions(plan *buildsystem.Plan, stage buildsystem.Stage) []buildsystem.Action {
	goModules := "download"
	if configured, ok := stageSettingString(stage, "goModules"); ok {
		goModules = configured
	}
	var actions []buildsystem.Action
	if goModules != "none" {
		arguments := map[string][]string{
			"validate": {"mod", "verify"},
			"download": {"mod", "download"},
			"tidy":     {"mod", "tidy"},
		}[goModules]
		if arguments == nil {
			arguments = []string{"mod", goModules}
		}
		actions = append(actions, buildsystem.Action{
			Kind:             buildsystem.ActionCommand,
			Description:      "Prepare Go module dependencies (" + goModules + ")",
			Status:           "planned",
			Command:          append([]string{"go"}, arguments...),
			WorkingDirectory: plan.Project.Root,
		})
	}
	frontendPolicy := "install-if-needed"
	if configured, ok := stageSettingString(stage, "frontend"); ok {
		frontendPolicy = configured
	}
	for _, candidate := range plan.Stages {
		if candidate.Operation() == "frontend.build" {
			if _, configured := stageSettingCommand(candidate, "install"); configured {
				frontendPolicy = "none"
			}
			break
		}
	}
	if frontendPolicy == "none" {
		return actions
	}

	frontend := filepath.Join(plan.Project.Root, plan.Project.Frontend)
	if _, err := os.Stat(filepath.Join(frontend, "package.json")); os.IsNotExist(err) {
		return actions
	}
	packageManager := plan.Project.PackageManager
	for _, candidate := range plan.Stages {
		if candidate.Operation() == "frontend.build" {
			if configured, ok := stageSettingString(candidate, "packageManager"); ok {
				packageManager = configured
			}
			break
		}
	}
	name, args := frontendInstallCommand(frontend, packageManager)
	action := buildsystem.Action{
		Kind:             buildsystem.ActionCommand,
		Description:      "Install frontend dependencies",
		Status:           "planned",
		Command:          append([]string{name}, args...),
		WorkingDirectory: frontend,
	}
	if frontendPolicy == "install-if-needed" && frontendDependenciesExist(frontend) {
		action.Status = "skipped"
		action.Reason = "frontend dependencies are already installed"
	}
	return append(actions, action)
}

func nativeCompileActions(
	plan *buildsystem.Plan,
	stage buildsystem.Stage,
	buildFlags *flags.Build,
) ([]buildsystem.Action, error) {
	if stage.Target == nil {
		return nil, fmt.Errorf("stage %s has no target", stage.Reference())
	}
	output, err := planStageOutputPath(plan, stage, "binary")
	if err != nil {
		return nil, err
	}
	actions := []buildsystem.Action{{
		Kind:        buildsystem.ActionMkdir,
		Description: "Create the binary output directory",
		Status:      "planned",
		Path:        filepath.Dir(output),
	}}
	if buildFlags.Obfuscated && !buildFlags.Docker {
		actions = append(actions, buildsystem.Action{
			Kind: buildsystem.ActionCheckTool, Description: "Check the garble compiler", Status: "planned", Tool: "garble",
		})
	}
	if buildFlags.Docker {
		if !slices.Contains([]string{"windows", "darwin", "linux"}, stage.Target.Platform) {
			return nil, fmt.Errorf("Docker cross-compilation is not supported for %s targets", stage.Target.Platform)
		}
		mounts, err := dockerMountArguments(plan.Project.Root)
		if err != nil {
			return nil, err
		}
		image := buildFlags.DockerImage
		if image == "" {
			image = "wails-cross"
		}
		dockerCommand := []string{"docker", "run", "--rm", "-v", plan.Project.Root + ":/app"}
		dockerCommand = append(dockerCommand, mounts...)
		dockerCommand = append(dockerCommand, "-e", "APP_NAME="+plan.Project.BinaryName)
		if len(stage.Target.Tags) > 0 {
			dockerCommand = append(dockerCommand, "-e", "EXTRA_TAGS="+strings.Join(stage.Target.Tags, ","))
		}
		if buildFlags.Obfuscated {
			dockerCommand = append(dockerCommand, "-e", "OBFUSCATED=true")
			if buildFlags.GarbleArgs != "" {
				dockerCommand = append(dockerCommand, "-e", "GARBLE_ARGS="+buildFlags.GarbleArgs)
			}
		}
		dockerCommand = append(dockerCommand, image, stage.Target.Platform, stage.Target.Arch)
		generatedName := plan.Project.BinaryName + "-" + stage.Target.Platform + "-" + stage.Target.Arch
		if stage.Target.Platform == "windows" {
			generatedName += ".exe"
		}
		generated := filepath.Join(plan.Project.Root, "bin", generatedName)
		return append(actions,
			buildsystem.Action{Kind: buildsystem.ActionCheckTool, Description: "Check Docker", Status: "planned", Tool: "docker"},
			buildsystem.Action{Kind: buildsystem.ActionCommand, Description: "Check the Wails cross-build image", Status: "planned", Command: []string{"docker", "image", "inspect", image}, WorkingDirectory: plan.Project.Root},
			buildsystem.Action{Kind: buildsystem.ActionCommand, Description: "Compile the application in Docker", Status: "planned", Command: dockerCommand, WorkingDirectory: plan.Project.Root},
			buildsystem.Action{Kind: buildsystem.ActionCopy, Description: "Collect the Docker-built binary", Status: "planned", Source: generated, Destination: output},
			buildsystem.Action{Kind: buildsystem.ActionRemove, Description: "Remove the Docker intermediate binary", Status: "planned", Finally: true, Path: generated},
		), nil
	}
	if stage.Target.Platform == "android" {
		platform, err := stageInputPath(plan, stage, "platform")
		if err != nil {
			return nil, err
		}
		command, err := goBuildCommand(buildFlags, "-buildmode=c-shared", "-overlay", filepath.Join(platform, "overlay.json"))
		if err != nil {
			return nil, err
		}
		tags := appendUniqueStrings(stage.Target.Tags, "android")
		if len(tags) > 0 {
			command = append(command, "-tags", strings.Join(tags, ","))
		}
		command = append(command, "-trimpath", "-buildvcs=false", "-ldflags=-w -s", "-o", output)
		script, err := androidCompileScript(stage.Target.Arch, command)
		if err != nil {
			return nil, err
		}
		return append(actions, buildsystem.Action{
			Kind: buildsystem.ActionCommand, Description: "Compile the Android shared library", Status: "planned",
			Command: []string{"/bin/sh", "-c", script}, WorkingDirectory: plan.Project.Root,
			Environment: map[string]string{"GOOS": "android", "GOARCH": stage.Target.Arch, "CGO_ENABLED": "1"},
		}), nil
	}
	if stage.Target.Platform == "ios" {
		platform, err := stageInputPath(plan, stage, "platform")
		if err != nil {
			return nil, err
		}
		command, err := goBuildCommand(buildFlags, "-buildmode=c-archive", "-overlay", filepath.Join(platform, "overlay.json"))
		if err != nil {
			return nil, err
		}
		tags := appendUniqueStrings(stage.Target.Tags, "ios")
		if len(tags) > 0 {
			command = append(command, "-tags", strings.Join(tags, ","))
		}
		command = append(command, "-trimpath", "-buildvcs=false", "-ldflags=-w -s", "-o", output)
		target := "arm64-apple-ios13.0"
		sdk := "iphoneos"
		if stage.Target.Arch == "amd64" {
			target = "x86_64-apple-ios13.0-simulator"
			sdk = "iphonesimulator"
		}
		quoted := make([]string, len(command))
		for index, argument := range command {
			quoted[index] = shellQuote(argument)
		}
		script := "set -eu\nSDK_PATH=$(xcrun --sdk " + sdk + " --show-sdk-path)\n" +
			"export CGO_CFLAGS=\"-isysroot $SDK_PATH -target " + target + "\"\n" +
			"export CGO_LDFLAGS=\"-isysroot $SDK_PATH -target " + target + "\"\n" +
			"exec " + strings.Join(quoted, " ")
		return append(actions, buildsystem.Action{
			Kind: buildsystem.ActionCommand, Description: "Compile the iOS native archive", Status: "planned",
			Command: []string{"/bin/sh", "-c", script}, WorkingDirectory: plan.Project.Root,
			Environment: map[string]string{
				"GOOS": "ios", "GOARCH": stage.Target.Arch, "CGO_ENABLED": "1",
			},
		}), nil
	}

	var temporaryResource string
	if stage.Target.Platform == "windows" && plan.Variant != "server" {
		if platform, ok := optionalStageInputPath(plan, stage, "platform"); ok {
			name := "rsrc_windows_" + stage.Target.Arch + ".syso"
			temporaryResource = filepath.Join(plan.Project.Root, name)
			actions = append(actions, buildsystem.Action{
				Kind:        buildsystem.ActionCopy,
				Description: "Install the generated Windows resource for Go compilation",
				Status:      "planned",
				Source:      filepath.Join(platform, name),
				Destination: temporaryResource,
			})
		}
	}

	command, err := goBuildCommand(buildFlags)
	if err != nil {
		return nil, err
	}
	tags := stage.Target.Tags
	if configured, ok := stageSettingStrings(stage, "tags"); ok {
		tags = appendUniqueStrings(tags, configured...)
	}
	if len(tags) > 0 {
		command = append(command, "-tags", strings.Join(tags, ","))
	}
	trimPath := stageSettingBoolDefault(stage, "trimPath", plan.Mode != "development")
	vcsInfo := stageSettingBoolDefault(stage, "vcsInfo", false)
	if trimPath {
		command = append(command, "-trimpath")
	}
	command = append(command, "-buildvcs="+strconv.FormatBool(vcsInfo))
	if plan.Mode == "development" {
		command = append(command, "-gcflags=all=-l")
	} else {
		command = append(command, "-ldflags=-w -s")
	}
	command = append(command, "-o", output)
	environment := map[string]string{
		"GOOS":   stage.Target.Platform,
		"GOARCH": stage.Target.Arch,
	}
	if slices.Contains([]string{"linux", "darwin"}, stage.Target.Platform) {
		environment["CGO_ENABLED"] = "1"
	} else {
		environment["CGO_ENABLED"] = "0"
	}
	if stage.Target.Platform == "darwin" {
		environment["CGO_CFLAGS"] = "-mmacosx-version-min=12.0"
		environment["CGO_LDFLAGS"] = "-mmacosx-version-min=12.0"
		environment["MACOSX_DEPLOYMENT_TARGET"] = "12.0"
	}
	if plan.Variant == "server" {
		environment = map[string]string{"GOOS": stage.Target.Platform, "GOARCH": stage.Target.Arch, "CGO_ENABLED": "0"}
	}
	actions = append(actions, buildsystem.Action{
		Kind:             buildsystem.ActionCommand,
		Description:      "Compile the native application binary",
		Status:           "planned",
		Command:          command,
		WorkingDirectory: plan.Project.Root,
		Environment:      environment,
	})
	if temporaryResource != "" {
		actions = append(actions, buildsystem.Action{
			Kind:        buildsystem.ActionRemove,
			Description: "Remove the temporary Windows resource",
			Status:      "planned",
			Finally:     true,
			Path:        temporaryResource,
		})
	}
	return actions, nil
}

func goBuildCommand(buildFlags *flags.Build, arguments ...string) ([]string, error) {
	if !buildFlags.Obfuscated {
		return append([]string{"go", "build"}, arguments...), nil
	}
	garbleArguments, err := shlex.Split(buildFlags.GarbleArgs)
	if err != nil {
		return nil, fmt.Errorf("parse garble arguments: %w", err)
	}
	command := append([]string{"garble"}, garbleArguments...)
	command = append(command, "build")
	return append(command, arguments...), nil
}

func androidCompileScript(arch string, command []string) (string, error) {
	triple := map[string]string{"arm64": "aarch64-linux-android", "amd64": "x86_64-linux-android", "386": "i686-linux-android"}[arch]
	if triple == "" {
		return "", fmt.Errorf("unsupported Android architecture %q", arch)
	}
	host := map[string]string{"darwin": "darwin-x86_64", "linux": "linux-x86_64"}[runtime.GOOS]
	if host == "" {
		return "", fmt.Errorf("Android cross-compilation is unsupported on host %q", runtime.GOOS)
	}
	quoted := make([]string, len(command))
	for index, argument := range command {
		quoted[index] = shellQuote(argument)
	}
	return "set -eu\n" +
		`NDK_ROOT="${ANDROID_NDK_HOME:-}"` + "\n" +
		`if [ -z "$NDK_ROOT" ]; then SDK_ROOT="${ANDROID_SDK_ROOT:-${ANDROID_HOME:-}}"; NDK_ROOT=$(find "$SDK_ROOT/ndk" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | sort -V | tail -1); fi` + "\n" +
		`if [ -z "$NDK_ROOT" ] || [ ! -d "$NDK_ROOT" ]; then echo "Android NDK not found; set ANDROID_NDK_HOME" >&2; exit 1; fi` + "\n" +
		"export CC=\"$NDK_ROOT/toolchains/llvm/prebuilt/" + host + "/bin/" + triple + "21-clang\"\n" +
		"exec " + strings.Join(quoted, " "), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func binaryCombineActions(plan *buildsystem.Plan, stage buildsystem.Stage) ([]buildsystem.Action, error) {
	output, err := planStageOutputPath(plan, stage, "binary")
	if err != nil {
		return nil, err
	}
	inputs := make([]string, 0, len(stage.Inputs))
	for _, artifact := range stage.Inputs {
		if artifact.Name == "binary" {
			inputs = append(inputs, absoluteArtifactPath(plan.Project.Root, artifact))
		}
	}
	if len(inputs) < 2 {
		return nil, fmt.Errorf("stage %s requires at least two native binaries", stage.Reference())
	}
	encoded, err := json.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("encode universal binary inputs: %w", err)
	}
	return []buildsystem.Action{
		{
			Kind:        buildsystem.ActionMkdir,
			Description: "Create the universal binary output directory",
			Status:      "planned",
			Path:        filepath.Dir(output),
		},
		internalBuildActionWithParameters(
			"binary.combine",
			"Combine architecture-specific binaries into a universal binary",
			map[string]string{
				"inputs": string(encoded),
				"output": output,
			},
		),
	}, nil
}

func bundleAssembleActions(plan *buildsystem.Plan, stage buildsystem.Stage) ([]buildsystem.Action, error) {
	if stage.Target == nil {
		return nil, fmt.Errorf("stage %s has no target", stage.Reference())
	}
	bundle, err := planStageOutputPath(plan, stage, "bundle")
	if err != nil {
		return nil, err
	}
	binary, err := stageInputPath(plan, stage, "binary")
	if err != nil {
		return nil, err
	}
	var darwinPlatform string
	switch stage.Target.Platform {
	case "windows":
		actions := []buildsystem.Action{
			{Kind: buildsystem.ActionRemove, Description: "Remove the previous Windows application layout", Status: "planned", Recursive: true, Path: bundle},
			{Kind: buildsystem.ActionMkdir, Description: "Create the Windows application layout", Status: "planned", Path: bundle},
			{Kind: buildsystem.ActionCopy, Description: "Install the Windows executable", Status: "planned", Source: binary, Destination: filepath.Join(bundle, filepath.Base(binary))},
		}
		if assets, ok := optionalStageInputPath(plan, stage, "assets"); ok {
			actions = append(actions, buildsystem.Action{Kind: buildsystem.ActionCopy, Description: "Install the Windows application icon", Status: "planned", Optional: true, Source: filepath.Join(assets, "icon.ico"), Destination: filepath.Join(bundle, "icon.ico")})
		}
		return actions, nil
	case "linux":
		usrBin := filepath.Join(bundle, "usr", "bin")
		applications := filepath.Join(bundle, "usr", "share", "applications")
		icons := filepath.Join(bundle, "usr", "share", "icons", "hicolor", "256x256", "apps")
		actions := []buildsystem.Action{
			{Kind: buildsystem.ActionRemove, Description: "Remove the previous Linux AppDir", Status: "planned", Recursive: true, Path: bundle},
			{Kind: buildsystem.ActionMkdir, Description: "Create the Linux executable directory", Status: "planned", Path: usrBin},
			{Kind: buildsystem.ActionMkdir, Description: "Create the Linux desktop entry directory", Status: "planned", Path: applications},
			{Kind: buildsystem.ActionMkdir, Description: "Create the Linux icon directory", Status: "planned", Path: icons},
			{Kind: buildsystem.ActionCopy, Description: "Install the Linux executable", Status: "planned", Source: binary, Destination: filepath.Join(usrBin, plan.Project.BinaryName)},
			{Kind: buildsystem.ActionCopy, Description: "Install the AppImage launcher", Status: "planned", Source: binary, Destination: filepath.Join(bundle, "AppRun")},
			{Kind: buildsystem.ActionCopy, Description: "Install the optional Linux desktop entry", Status: "planned", Optional: true, Source: filepath.Join(plan.Project.Root, "build", "linux", plan.Project.BinaryName+".desktop"), Destination: filepath.Join(applications, plan.Project.BinaryName+".desktop")},
			{Kind: buildsystem.ActionCopy, Description: "Install the optional AppImage desktop entry", Status: "planned", Optional: true, Source: filepath.Join(plan.Project.Root, "build", "linux", plan.Project.BinaryName+".desktop"), Destination: filepath.Join(bundle, plan.Project.BinaryName+".desktop")},
		}
		if assets, ok := optionalStageInputPath(plan, stage, "assets"); ok {
			actions = append(actions,
				buildsystem.Action{Kind: buildsystem.ActionCopy, Description: "Install the Linux application icon", Status: "planned", Optional: true, Source: filepath.Join(assets, "appicon.png"), Destination: filepath.Join(icons, plan.Project.BinaryName+".png")},
				buildsystem.Action{Kind: buildsystem.ActionCopy, Description: "Install the optional AppImage root icon", Status: "planned", Optional: true, Source: filepath.Join(assets, "appicon.png"), Destination: filepath.Join(bundle, plan.Project.BinaryName+".png")},
			)
		}
		return actions, nil
	case "android":
		gradleProject := filepath.Join(plan.Project.Root, "build", "android")
		abi := map[string]string{"arm64": "arm64-v8a", "amd64": "x86_64", "386": "x86"}[stage.Target.Arch]
		if abi == "" {
			return nil, fmt.Errorf("unsupported Android architecture %q", stage.Target.Arch)
		}
		jni := filepath.Join(bundle, "app", "src", "main", "jniLibs", abi)
		return []buildsystem.Action{
			{Kind: buildsystem.ActionRemove, Description: "Remove the previous Android staging project", Status: "planned", Recursive: true, Path: bundle},
			{Kind: buildsystem.ActionCopy, Description: "Stage the Android Gradle project", Status: "planned", Recursive: true, Source: gradleProject, Destination: bundle},
			{Kind: buildsystem.ActionMkdir, Description: "Create the Android JNI library directory", Status: "planned", Path: jni},
			{Kind: buildsystem.ActionCopy, Description: "Install the Android shared library", Status: "planned", Source: binary, Destination: filepath.Join(jni, "libwails.so")},
		}, nil
	case "ios":
		platform, err := stageInputPath(plan, stage, "platform")
		if err != nil {
			return nil, err
		}
		target := "arm64-apple-ios13.0"
		sdk := "iphoneos"
		if stage.Target.Arch == "amd64" {
			target = "x86_64-apple-ios13.0-simulator"
			sdk = "iphonesimulator"
		}
		mainDirectory := filepath.Join(platform, "main")
		executable := filepath.Join(bundle, strings.ToLower(plan.Project.BinaryName))
		infoPlist := filepath.Join(mainDirectory, "Info.plist")
		if plan.Mode == "development" {
			if candidate := filepath.Join(plan.Project.Root, "build", "ios", "Info.dev.plist"); fileExists(candidate) {
				infoPlist = candidate
			}
		}
		actions := []buildsystem.Action{
			{Kind: buildsystem.ActionRemove, Description: "Remove the previous iOS application bundle", Status: "planned", Recursive: true, Path: bundle},
			{Kind: buildsystem.ActionMkdir, Description: "Create the iOS application bundle", Status: "planned", Path: bundle},
			{Kind: buildsystem.ActionCommand, Description: "Link the iOS application executable", Status: "planned",
				Command: []string{"xcrun", "-sdk", sdk, "clang", "-target", target,
					"-framework", "Foundation", "-framework", "UIKit", "-framework", "WebKit", "-framework", "Security",
					"-framework", "CoreFoundation", "-framework", "UniformTypeIdentifiers", "-framework", "LocalAuthentication",
					"-framework", "UserNotifications", "-framework", "AVFoundation", "-framework", "CoreLocation",
					"-framework", "CoreMotion", "-framework", "SystemConfiguration", "-lresolv",
					"-o", executable, filepath.Join(mainDirectory, "main.m"), "-Wl,-force_load," + binary},
				WorkingDirectory: plan.Project.Root,
			},
			{Kind: buildsystem.ActionCopy, Description: "Install the iOS application property list", Status: "planned", Source: infoPlist, Destination: filepath.Join(bundle, "Info.plist")},
			{Kind: buildsystem.ActionCommand, Description: "Compile the iOS asset catalog", Status: "planned",
				Command: []string{"xcrun", "actool", "--compile", bundle, "--app-icon", "AppIcon", "--platform", sdk,
					"--minimum-deployment-target", "13.0", "--product-type", "com.apple.product-type.application",
					"--target-device", "iphone", "--target-device", "ipad", "--output-partial-info-plist",
					filepath.Join(bundle, "assetcatalog_generated_info.plist"), filepath.Join(mainDirectory, "Assets.xcassets")},
				WorkingDirectory: plan.Project.Root,
			},
			{Kind: buildsystem.ActionCommand, Description: "Merge generated iOS asset metadata", Status: "planned",
				Command:          []string{"/usr/libexec/PlistBuddy", "-c", "Merge " + filepath.Join(bundle, "assetcatalog_generated_info.plist"), filepath.Join(bundle, "Info.plist")},
				WorkingDirectory: plan.Project.Root,
			},
		}
		identity := "-"
		description := "Ad-hoc sign the iOS simulator bundle"
		if stage.Target.Arch == "arm64" {
			identity = plan.Signing.IOS.Identity
			if identity == "" {
				identity = "-"
				description = "Ad-hoc sign the iOS device bundle"
			} else {
				description = "Sign the iOS device bundle"
			}
			if profile := absoluteConfiguredPath(plan.Project.Root, plan.Signing.IOS.ProvisioningProfile); profile != "" {
				actions = append(actions, buildsystem.Action{Kind: buildsystem.ActionCopy, Description: "Embed the iOS provisioning profile", Status: "planned", Source: profile, Destination: filepath.Join(bundle, "embedded.mobileprovision")})
			}
		}
		signCommand := []string{"codesign", "--force", "--sign", identity}
		if entitlements := absoluteConfiguredPath(plan.Project.Root, plan.Signing.IOS.Entitlements); stage.Target.Arch == "arm64" && entitlements != "" {
			signCommand = append(signCommand, "--entitlements", entitlements)
		}
		signCommand = append(signCommand, bundle)
		actions = append(actions, buildsystem.Action{Kind: buildsystem.ActionCommand, Description: description, Status: "planned", Command: signCommand, WorkingDirectory: plan.Project.Root})
		return actions, nil
	case "darwin":
		platform, ok := optionalStageInputPath(plan, stage, "platform")
		if !ok {
			platform = filepath.Join(plan.Project.Root, "build", "darwin")
		}
		darwinPlatform = platform
	default:
		return nil, fmt.Errorf("stage %s has unsupported bundle target %q", stage.Reference(), stage.Target.Platform)
	}
	macOS := filepath.Join(bundle, "Contents", "MacOS")
	resources := filepath.Join(bundle, "Contents", "Resources")
	actions := []buildsystem.Action{
		{
			Kind:        buildsystem.ActionRemove,
			Description: "Remove the previous macOS application bundle",
			Status:      "planned",
			Recursive:   true,
			Path:        bundle,
		},
		{
			Kind:        buildsystem.ActionMkdir,
			Description: "Create the macOS executable directory",
			Status:      "planned",
			Path:        macOS,
		},
		{
			Kind:        buildsystem.ActionMkdir,
			Description: "Create the macOS resource directory",
			Status:      "planned",
			Path:        resources,
		},
		{
			Kind:        buildsystem.ActionCopy,
			Description: "Install the native binary in the application bundle",
			Status:      "planned",
			Source:      binary,
			Destination: filepath.Join(macOS, filepath.Base(binary)),
		},
		{
			Kind:        buildsystem.ActionCopy,
			Description: "Install the macOS application property list",
			Status:      "planned",
			Source:      filepath.Join(darwinPlatform, "Info.plist"),
			Destination: filepath.Join(bundle, "Contents", "Info.plist"),
		},
		{
			Kind:        buildsystem.ActionCopy,
			Description: "Install the macOS application icon bundle",
			Status:      "planned",
			Source:      filepath.Join(darwinPlatform, "icons.icns"),
			Destination: filepath.Join(resources, "icons.icns"),
		},
		{
			Kind:        buildsystem.ActionCopy,
			Description: "Install the optional macOS compiled asset catalog",
			Status:      "planned",
			Optional:    true,
			Source:      filepath.Join(plan.Project.Root, "build", "darwin", "Assets.car"),
			Destination: filepath.Join(resources, "Assets.car"),
		},
	}
	if plan.Mode == "development" {
		actions = append(actions, buildsystem.Action{
			Kind: buildsystem.ActionCommand, Description: "Ad-hoc sign the development application bundle", Status: "planned",
			Command: []string{"codesign", "--force", "--deep", "--sign", "-", bundle}, WorkingDirectory: plan.Project.Root,
		})
	}
	return actions, nil
}

func packageCreateActions(plan *buildsystem.Plan, stage buildsystem.Stage) ([]buildsystem.Action, error) {
	if stage.Target == nil || len(stage.Inputs) != 1 || len(stage.Outputs) != 1 {
		return nil, fmt.Errorf("stage %s has an invalid package contract", stage.Reference())
	}
	input := absoluteArtifactPath(plan.Project.Root, stage.Inputs[0])
	output := absoluteArtifactPath(plan.Project.Root, stage.Outputs[0])
	format := stage.Outputs[0].Target.Format
	actions := []buildsystem.Action{{
		Kind:        buildsystem.ActionMkdir,
		Description: "Create the distribution output directory",
		Status:      "planned",
		Path:        filepath.Dir(output),
	}}
	switch format {
	case "zip":
		return append(actions, internalBuildActionWithParameters(
			"package.zip",
			"Create a ZIP distribution archive",
			map[string]string{"input": input, "output": output},
		)), nil
	case "deb", "rpm", "archlinux":
		if stage.Target.Platform != "linux" {
			return nil, fmt.Errorf("package format %q is only supported for Linux targets", format)
		}
		input = bundledExecutable(input, stage.Target.Platform, plan.Project.BinaryName)
		return append(actions, internalBuildActionWithParameters(
			"package.linux",
			"Create a Linux distribution package with nfpm",
			map[string]string{
				"arch":   stage.Target.Arch,
				"config": filepath.Join(plan.Project.Root, "build", "linux", "nfpm", "nfpm.yaml"),
				"format": format,
				"input":  input,
				"name":   plan.Project.BinaryName,
				"output": output,
			},
		)), nil
	case "nsis":
		if stage.Target.Platform != "windows" {
			return nil, fmt.Errorf("package format %q is only supported for Windows targets", format)
		}
		input = bundledExecutable(input, stage.Target.Platform, plan.Project.BinaryName)
		define := "ARG_WAILS_" + strings.ToUpper(stage.Target.Arch) + "_BINARY"
		actions = append(actions, buildsystem.Action{
			Kind:             buildsystem.ActionCommand,
			Description:      "Create the Windows NSIS installer",
			Status:           "planned",
			Command:          []string{"makensis", "-D" + define + "=" + input, "project.nsi"},
			WorkingDirectory: filepath.Join(plan.Project.Root, "build", "windows", "nsis"),
		})
		generated := filepath.Join(
			plan.Project.Root,
			plan.Project.Output,
			plan.Project.BinaryName+"-"+strings.ToUpper(stage.Target.Arch)+"-installer.exe",
		)
		if filepath.Clean(generated) != filepath.Clean(output) {
			actions = append(actions,
				buildsystem.Action{
					Kind:        buildsystem.ActionCopy,
					Description: "Move the NSIS installer into its matrix output directory",
					Status:      "planned",
					Source:      generated,
					Destination: output,
				},
				buildsystem.Action{
					Kind:        buildsystem.ActionRemove,
					Description: "Remove the intermediate NSIS installer",
					Status:      "planned",
					Finally:     true,
					Path:        generated,
				},
			)
		}
		return actions, nil
	case "msix":
		if stage.Target.Platform != "windows" {
			return nil, fmt.Errorf("package format %q is only supported for Windows targets", format)
		}
		return append(actions,
			buildsystem.Action{
				Kind: buildsystem.ActionCopy, Description: "Install the MSIX application manifest", Status: "planned",
				Source: filepath.Join(plan.Project.Root, "build", "windows", "AppxManifest.xml"), Destination: filepath.Join(input, "AppxManifest.xml"),
			},
			buildsystem.Action{
				Kind: buildsystem.ActionCommand, Description: "Create the Windows MSIX package", Status: "planned",
				Command: []string{"makeappx.exe", "pack", "/d", input, "/p", output, "/o"}, WorkingDirectory: plan.Project.Root,
			},
		), nil
	case "appimage":
		if stage.Target.Platform != "linux" {
			return nil, fmt.Errorf("package format %q is only supported for Linux targets", format)
		}
		return append(actions, buildsystem.Action{
			Kind: buildsystem.ActionCommand, Description: "Create the Linux AppImage", Status: "planned",
			Command: []string{"appimagetool", input, output}, WorkingDirectory: plan.Project.Root,
		}), nil
	case "dmg":
		if stage.Target.Platform != "darwin" {
			return nil, fmt.Errorf("package format %q is only supported for macOS targets", format)
		}
		return append(actions, buildsystem.Action{
			Kind: buildsystem.ActionCommand, Description: "Create the macOS disk image", Status: "planned",
			Command: []string{"hdiutil", "create", "-volname", plan.Project.Name, "-srcfolder", input, "-ov", "-format", "UDZO", output}, WorkingDirectory: plan.Project.Root,
		}), nil
	case "pkg":
		if stage.Target.Platform != "darwin" {
			return nil, fmt.Errorf("package format %q is only supported for macOS targets", format)
		}
		return append(actions, buildsystem.Action{
			Kind: buildsystem.ActionCommand, Description: "Create the macOS installer package", Status: "planned",
			Command: []string{"productbuild", "--component", input, "/Applications", output}, WorkingDirectory: plan.Project.Root,
		}), nil
	case "apk", "aab":
		if stage.Target.Platform != "android" {
			return nil, fmt.Errorf("package format %q is only supported for Android targets", format)
		}
		gradleTask := "assembleRelease"
		generated := filepath.Join(input, "app", "build", "outputs", "apk", "release", "app-release.apk")
		if plan.Mode == "development" {
			gradleTask = "assembleDebug"
			generated = filepath.Join(input, "app", "build", "outputs", "apk", "debug", "app-debug.apk")
		}
		if format == "aab" {
			gradleTask = "bundleRelease"
			generated = filepath.Join(input, "app", "build", "outputs", "bundle", "release", "app-release.aab")
			if plan.Mode == "development" {
				gradleTask = "bundleDebug"
				generated = filepath.Join(input, "app", "build", "outputs", "bundle", "debug", "app-debug.aab")
			}
		}
		return append(actions,
			buildsystem.Action{
				Kind: buildsystem.ActionCommand, Description: "Build the Android " + strings.ToUpper(format) + " with Gradle", Status: "planned",
				Command: []string{"/bin/sh", "-c", "chmod +x ./gradlew && ./gradlew " + gradleTask}, WorkingDirectory: input,
			},
			buildsystem.Action{Kind: buildsystem.ActionCopy, Description: "Collect the Android package", Status: "planned", Source: generated, Destination: output},
		), nil
	case "ipa":
		if stage.Target.Platform != "ios" {
			return nil, fmt.Errorf("package format %q is only supported for iOS targets", format)
		}
		payload := filepath.Join(plan.Project.Root, ".wails", "build", "package", buildsystem.SafeInstanceName(stage.Reference()), "Payload")
		return append(actions,
			buildsystem.Action{Kind: buildsystem.ActionRemove, Description: "Remove the previous iOS package staging directory", Status: "planned", Recursive: true, Path: filepath.Dir(payload)},
			buildsystem.Action{Kind: buildsystem.ActionMkdir, Description: "Create the iOS Payload directory", Status: "planned", Path: payload},
			buildsystem.Action{Kind: buildsystem.ActionCopy, Description: "Stage the iOS application bundle", Status: "planned", Recursive: true, Source: input, Destination: filepath.Join(payload, filepath.Base(input))},
			internalBuildActionWithParameters("package.zip", "Create the iOS IPA archive", map[string]string{"input": payload, "output": output}),
			buildsystem.Action{Kind: buildsystem.ActionRemove, Description: "Remove the iOS package staging directory", Status: "planned", Finally: true, Recursive: true, Path: filepath.Dir(payload)},
		), nil
	default:
		return nil, fmt.Errorf("unsupported package format %q for %s", format, stage.Target.Platform)
	}
}

func bundledExecutable(bundle, platform, name string) string {
	switch platform {
	case "windows":
		if !strings.EqualFold(filepath.Ext(name), ".exe") {
			name += ".exe"
		}
		return filepath.Join(bundle, name)
	case "linux":
		return filepath.Join(bundle, "usr", "bin", name)
	default:
		return bundle
	}
}

func signingActions(
	plan *buildsystem.Plan,
	stage buildsystem.Stage,
	options *flags.Sign,
) ([]buildsystem.Action, error) {
	if len(stage.Inputs) != 1 {
		return nil, fmt.Errorf("stage %s has an invalid signing contract", stage.Reference())
	}
	parameters := signingParameters(options)
	input := absoluteArtifactPath(plan.Project.Root, stage.Inputs[0])
	if stage.Target != nil && stage.Inputs[0].Type == "application-bundle" && stage.Target.Platform == "windows" {
		input = bundledExecutable(input, "windows", plan.Project.BinaryName)
	}
	parameters["input"] = input
	return []buildsystem.Action{internalBuildActionWithParameters(
		"artifact.sign",
		"Sign the resolved build artifact",
		parameters,
	)}, nil
}

func notarizeActions(
	plan *buildsystem.Plan,
	stage buildsystem.Stage,
	options *flags.Sign,
) ([]buildsystem.Action, error) {
	var bundle string
	var packageArtifact *buildsystem.Artifact
	for _, input := range stage.Inputs {
		if input.Type == "application-bundle" {
			bundle = absoluteArtifactPath(plan.Project.Root, input)
		}
		if input.Type == "distribution-package" {
			artifact := input
			packageArtifact = &artifact
		}
	}
	if bundle == "" {
		return nil, fmt.Errorf("stage %s has no application bundle to notarize", stage.Reference())
	}
	parameters := signingParameters(options)
	parameters["input"] = bundle
	actions := []buildsystem.Action{internalBuildActionWithParameters(
		"artifact.notarize",
		"Submit the macOS application for notarization and staple its ticket",
		parameters,
	)}
	if packageArtifact != nil && packageArtifact.Target != nil && packageArtifact.Target.Format == "zip" {
		actions = append(actions, internalBuildActionWithParameters(
			"package.zip",
			"Recreate the ZIP distribution with the stapled application bundle",
			map[string]string{
				"input":  bundle,
				"output": absoluteArtifactPath(plan.Project.Root, *packageArtifact),
			},
		))
	}
	return actions, nil
}

func signingParameters(options *flags.Sign) map[string]string {
	result := make(map[string]string)
	if options == nil {
		return result
	}
	result["certificate"] = options.Certificate
	result["thumbprint"] = options.Thumbprint
	result["timestamp"] = options.Timestamp
	result["identity"] = options.Identity
	result["entitlements"] = options.Entitlements
	result["hardenedRuntime"] = strconv.FormatBool(options.HardenedRuntime)
	result["keychainProfile"] = options.KeychainProfile
	result["pgpKey"] = options.PGPKey
	result["role"] = options.Role
	return result
}

func resolvePipelineSigningOptions(plan *buildsystem.Plan, provided *flags.Sign) *flags.Sign {
	result := &flags.Sign{}
	if provided != nil {
		*result = *provided
	}
	if result.Identity == "" {
		if len(plan.Targets) == 1 && plan.Targets[0].Platform == "ios" {
			result.Identity = plan.Signing.IOS.Identity
		} else {
			result.Identity = plan.Signing.Darwin.Identity
		}
	}
	if result.Entitlements == "" {
		if len(plan.Targets) == 1 && plan.Targets[0].Platform == "ios" {
			result.Entitlements = plan.Signing.IOS.Entitlements
		} else {
			result.Entitlements = plan.Signing.Darwin.Entitlements
		}
	}
	if result.KeychainProfile == "" {
		result.KeychainProfile = plan.Signing.Darwin.KeychainProfile
	}
	if result.Certificate == "" {
		result.Certificate = plan.Signing.Windows.Certificate
	}
	if result.Thumbprint == "" {
		result.Thumbprint = plan.Signing.Windows.Thumbprint
	}
	if result.Timestamp == "" {
		result.Timestamp = plan.Signing.Windows.TimestampServer
	}
	if result.PGPKey == "" {
		result.PGPKey = plan.Signing.Linux.PGPKey
	}
	if result.Role == "" {
		result.Role = plan.Signing.Linux.Role
	}
	if plan.Signing.Credentials.Provider == "environment" {
		resolveEnvironmentSigningOptions(result, plan.Signing.Credentials.Prefix)
	}
	resolveSigningDefaults(result)
	result.Entitlements = absoluteConfiguredPath(plan.Project.Root, result.Entitlements)
	result.Certificate = absoluteConfiguredPath(plan.Project.Root, result.Certificate)
	result.PGPKey = absoluteConfiguredPath(plan.Project.Root, result.PGPKey)
	if plan.Goal == "notarize" {
		result.HardenedRuntime = true
		result.Notarize = false
	}
	return result
}

func resolveEnvironmentSigningOptions(options *flags.Sign, prefix string) {
	if prefix == "" {
		prefix = "WAILS_SIGN_"
	}
	values := []struct {
		value *string
		name  string
	}{
		{&options.Certificate, "CERTIFICATE"}, {&options.Thumbprint, "THUMBPRINT"},
		{&options.Timestamp, "TIMESTAMP"}, {&options.Identity, "IDENTITY"},
		{&options.Entitlements, "ENTITLEMENTS"}, {&options.KeychainProfile, "KEYCHAIN_PROFILE"},
		{&options.PGPKey, "PGP_KEY"}, {&options.Role, "ROLE"},
	}
	for _, candidate := range values {
		if *candidate.value == "" {
			*candidate.value = os.Getenv(prefix + candidate.name)
		}
	}
}

func absoluteConfiguredPath(root, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, filepath.FromSlash(path))
}

func internalBuildAction(name, description string) buildsystem.Action {
	return internalBuildActionWithParameters(name, description, nil)
}

func internalBuildActionWithParameters(
	name string,
	description string,
	parameters map[string]string,
) buildsystem.Action {
	return buildsystem.Action{
		Kind:        buildsystem.ActionInternal,
		Description: description,
		Status:      "planned",
		Internal:    name,
		Parameters:  parameters,
	}
}

func planStageOutputPath(plan *buildsystem.Plan, stage buildsystem.Stage, name string) (string, error) {
	for _, artifact := range stage.Outputs {
		if artifact.Name == name {
			return absoluteArtifactPath(plan.Project.Root, artifact), nil
		}
	}
	return "", fmt.Errorf("stage %s has no output artifact %q", stage.ID, name)
}

func stageInputPath(plan *buildsystem.Plan, stage buildsystem.Stage, name string) (string, error) {
	for _, artifact := range stage.Inputs {
		if artifact.Name == name {
			return absoluteArtifactPath(plan.Project.Root, artifact), nil
		}
	}
	return "", fmt.Errorf("stage %s has no input artifact %q", stage.Reference(), name)
}

func optionalStageInputPath(plan *buildsystem.Plan, stage buildsystem.Stage, name string) (string, bool) {
	path, err := stageInputPath(plan, stage, name)
	return path, err == nil
}

func planTags(plan *buildsystem.Plan) []string {
	var tags []string
	for _, target := range plan.Targets {
		for _, tag := range target.Tags {
			if !slices.Contains(tags, tag) {
				tags = append(tags, tag)
			}
		}
	}
	return tags
}

func buildInternalActions(_ *flags.Build) map[string]buildsystem.ActionFunc {
	return map[string]buildsystem.ActionFunc{
		"bindings.generate":             generateBindingsAction,
		"assets.generate-icons":         generateIconsAction,
		"platform.generate-syso":        generateSysoAction,
		"platform.generate-android":     AndroidPlatformAction,
		"platform.generate-ios-overlay": IOSOverlayPlatformAction,
		"platform.generate-ios-xcode":   IOSXcodePlatformAction,
		"binary.combine":                combineBinaryAction,
		"package.zip":                   createZipPackageAction,
		"package.linux":                 createLinuxPackageAction,
		"artifact.sign":                 signArtifactAction,
		"artifact.notarize":             notarizeArtifactAction,
		"artifacts.collect":             collectArtifactsAction,
	}
}

func AndroidPlatformAction(
	_ context.Context,
	_ *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	return AndroidOverlayGen(&AndroidOverlayGenOptions{Out: action.Parameters["output"], Config: action.Parameters["config"]})
}

func IOSOverlayPlatformAction(
	_ context.Context,
	_ *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	return IOSOverlayGen(&IOSOverlayGenOptions{Out: action.Parameters["output"], Config: action.Parameters["config"]})
}

func IOSXcodePlatformAction(
	_ context.Context,
	_ *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	return IOSXcodeGen(&IOSXcodeGenOptions{OutDir: action.Parameters["output"], Config: action.Parameters["config"]})
}

func combineBinaryAction(
	_ context.Context,
	_ *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	var inputs []string
	if err := json.Unmarshal([]byte(action.Parameters["inputs"]), &inputs); err != nil {
		return fmt.Errorf("decode universal binary inputs: %w", err)
	}
	return ToolLipo(&flags.Lipo{Inputs: inputs, Output: action.Parameters["output"]})
}

func createZipPackageAction(
	ctx context.Context,
	_ *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	return createZipArtifact(ctx, action.Parameters["input"], action.Parameters["output"])
}

func createZipArtifact(ctx context.Context, input, output string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	file, err := os.Create(output)
	if err != nil {
		return err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = os.Remove(output)
		}
	}()
	archive := zip.NewWriter(file)
	base := filepath.Dir(input)
	err = filepath.Walk(input, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(name)
		header.SetMode(info.Mode())
		header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
		if info.IsDir() {
			header.Name += "/"
			_, err = archive.CreateHeader(header)
			return err
		}
		header.Method = zip.Deflate
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, err = io.WriteString(writer, target)
			return err
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, source)
		closeErr := source.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeArchiveErr := archive.Close()
	closeFileErr := file.Close()
	if err != nil {
		return err
	}
	if closeArchiveErr != nil {
		return closeArchiveErr
	}
	if closeFileErr != nil {
		return closeFileErr
	}
	succeeded = true
	return nil
}

func createLinuxPackageAction(
	_ context.Context,
	_ *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	data, err := os.ReadFile(action.Parameters["config"])
	if err != nil {
		return fmt.Errorf("read nfpm config: %w", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse nfpm config: %w", err)
	}
	rewritePackageConfig(
		&document,
		action.Parameters["name"],
		action.Parameters["input"],
		action.Parameters["arch"],
	)
	generated, err := yaml.Marshal(&document)
	if err != nil {
		return fmt.Errorf("encode resolved nfpm config: %w", err)
	}
	configPath := filepath.Join(
		filepath.Dir(action.Parameters["output"]),
		".wails-"+action.Parameters["format"]+"-nfpm.yaml",
	)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(configPath, generated, 0o644); err != nil {
		return err
	}
	defer os.Remove(configPath)

	var packageType packager.PackageType
	switch action.Parameters["format"] {
	case "deb":
		packageType = packager.DEB
	case "rpm":
		packageType = packager.RPM
	case "archlinux":
		packageType = packager.ARCH
	default:
		return fmt.Errorf("unsupported Linux package format %q", action.Parameters["format"])
	}
	return packager.CreatePackageFromConfig(packageType, configPath, action.Parameters["output"])
}

func rewritePackageConfig(node *yaml.Node, name, input, arch string) {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		node.Value = strings.ReplaceAll(node.Value, "${GOARCH}", arch)
		if node.Value == "./bin/"+name || node.Value == "bin/"+name {
			node.Value = filepath.ToSlash(input)
		}
	}
	for _, child := range node.Content {
		rewritePackageConfig(child, name, input, arch)
	}
}

func signArtifactAction(
	_ context.Context,
	_ *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	return Sign(signOptionsFromAction(action))
}

func notarizeArtifactAction(
	_ context.Context,
	_ *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	options := signOptionsFromAction(action)
	resolveSigningDefaults(options)
	return notarizeMacOSApp(options)
}

func signOptionsFromAction(action buildsystem.Action) *flags.Sign {
	hardened, _ := strconv.ParseBool(action.Parameters["hardenedRuntime"])
	return &flags.Sign{
		Input:           action.Parameters["input"],
		Certificate:     action.Parameters["certificate"],
		Thumbprint:      action.Parameters["thumbprint"],
		Timestamp:       action.Parameters["timestamp"],
		Identity:        action.Parameters["identity"],
		Entitlements:    action.Parameters["entitlements"],
		HardenedRuntime: hardened,
		KeychainProfile: action.Parameters["keychainProfile"],
		PGPKey:          action.Parameters["pgpKey"],
		Role:            action.Parameters["role"],
	}
}

func generateBindingsAction(
	_ context.Context,
	stage *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	typescript, err := strconv.ParseBool(action.Parameters["typescript"])
	if err != nil {
		return fmt.Errorf("parse bindings typescript setting: %w", err)
	}
	obfuscated, err := strconv.ParseBool(action.Parameters["obfuscated"])
	if err != nil {
		return fmt.Errorf("parse bindings obfuscated setting: %w", err)
	}
	clean, err := strconv.ParseBool(action.Parameters["clean"])
	if err != nil {
		return fmt.Errorf("parse bindings clean setting: %w", err)
	}
	options := &flags.GenerateBindingsOptions{
		BuildFlagsString: action.Parameters["buildFlags"],
		OutputDirectory:  action.Parameters["output"],
		ModelsFilename:   action.Parameters["models"],
		IndexFilename:    action.Parameters["index"],
		TimeType:         action.Parameters["timeType"],
		TS:               typescript,
		Obfuscated:       obfuscated,
		Clean:            clean,
		Silent:           true,
	}
	return GenerateBindings(options, []string{stage.Plan.Project.Root})
}

func generateIconsAction(
	_ context.Context,
	_ *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	options := &IconsOptions{
		Input: action.Parameters["input"],
		Sizes: action.Parameters["sizes"],
	}
	switch action.Parameters["target"] {
	case "windows":
		options.WindowsFilename = action.Parameters["output"]
	case "darwin":
		options.MacFilename = action.Parameters["output"]
	default:
		return fmt.Errorf("unsupported icon target %q", action.Parameters["target"])
	}
	return GenerateIcons(options)
}

func generateSysoAction(
	_ context.Context,
	_ *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	return GenerateSyso(&SysoOptions{
		Manifest: action.Parameters["manifest"],
		Info:     action.Parameters["info"],
		Icon:     action.Parameters["icon"],
		Out:      action.Parameters["output"],
		Arch:     action.Parameters["arch"],
	})
}

func frontendDependenciesExist(frontend string) bool {
	if _, err := os.Stat(filepath.Join(frontend, "node_modules")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(frontend, ".pnp.cjs")); err == nil {
		return true
	}
	return false
}

func frontendInstallCommand(frontend, packageManager string) (string, []string) {
	switch packageManager {
	case "npm":
		if fileExists(filepath.Join(frontend, "package-lock.json")) {
			return packageManager, []string{"ci"}
		}
		return packageManager, []string{"install", "--no-package-lock"}
	case "pnpm":
		if fileExists(filepath.Join(frontend, "pnpm-lock.yaml")) {
			return packageManager, []string{"install", "--frozen-lockfile"}
		}
	case "yarn":
		if fileExists(filepath.Join(frontend, "yarn.lock")) {
			return packageManager, []string{"install", "--immutable"}
		}
	case "bun":
		if fileExists(filepath.Join(frontend, "bun.lock")) || fileExists(filepath.Join(frontend, "bun.lockb")) {
			return packageManager, []string{"install", "--frozen-lockfile"}
		}
	}
	return packageManager, []string{"install"}
}

type buildArtifactManifest struct {
	Artifacts []buildArtifactEntry `json:"artifacts"`
}

type buildArtifactEntry struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	Format   string `json:"format,omitempty"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

type collectionInput struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	ReportPath string `json:"reportPath"`
	Platform   string `json:"platform"`
	Arch       string `json:"arch"`
	Format     string `json:"format,omitempty"`
}

func collectionInputs(plan *buildsystem.Plan, artifacts []buildsystem.Artifact) []collectionInput {
	result := make([]collectionInput, 0, len(artifacts))
	for _, artifact := range artifacts {
		input := collectionInput{
			ID:         artifact.Reference(),
			Type:       artifact.Type,
			Path:       absoluteArtifactPath(plan.Project.Root, artifact),
			ReportPath: artifact.Path,
		}
		if artifact.Target != nil {
			input.Platform = artifact.Target.Platform
			input.Arch = artifact.Target.Arch
			input.Format = artifact.Target.Format
		}
		result = append(result, input)
	}
	return result
}

func collectArtifactsAction(
	_ context.Context,
	stage *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	var inputs []collectionInput
	if err := json.Unmarshal([]byte(action.Parameters["artifacts"]), &inputs); err != nil {
		return fmt.Errorf("decode artifact collection inputs: %w", err)
	}
	entries := make([]buildArtifactEntry, 0, len(inputs))
	for _, input := range inputs {
		if _, ok := stage.Artifacts[input.ID]; !ok {
			return fmt.Errorf("artifact %q is not available to stage %s", input.ID, stage.Stage.Reference())
		}
		size, digest, err := hashBuildArtifact(input.Path)
		if err != nil {
			return err
		}
		entries = append(entries, buildArtifactEntry{
			ID:       input.ID,
			Type:     input.Type,
			Platform: input.Platform,
			Arch:     input.Arch,
			Format:   input.Format,
			Path:     input.ReportPath,
			Size:     size,
			SHA256:   digest,
		})
	}
	manifestPath := action.Parameters["manifest"]
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		return err
	}
	payload := buildArtifactManifest{Artifacts: entries}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, append(data, '\n'), 0o644)
}

func hashBuildArtifact(path string) (int64, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	hasher := sha256.New()
	if !info.IsDir() {
		file, err := os.Open(path)
		if err != nil {
			return 0, "", err
		}
		_, copyErr := io.Copy(hasher, file)
		closeErr := file.Close()
		if copyErr != nil {
			return 0, "", copyErr
		}
		if closeErr != nil {
			return 0, "", closeErr
		}
		return info.Size(), hex.EncodeToString(hasher.Sum(nil)), nil
	}

	var size int64
	err = filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(path, current)
		if err != nil {
			return err
		}
		_, _ = io.WriteString(hasher, filepath.ToSlash(relative)+"\x00"+entryInfo.Mode().String()+"\x00")
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(current)
			if err != nil {
				return err
			}
			_, _ = io.WriteString(hasher, target)
			return nil
		}
		if !entryInfo.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(current)
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(hasher, file)
		closeErr := file.Close()
		size += written
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hasher.Sum(nil)), nil
}

func absoluteArtifactPath(root string, artifact buildsystem.Artifact) string {
	if filepath.IsAbs(artifact.Path) {
		return artifact.Path
	}
	return filepath.Join(root, filepath.FromSlash(artifact.Path))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
