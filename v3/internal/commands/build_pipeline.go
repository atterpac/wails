package commands

import (
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

	"github.com/wailsapp/wails/v3/internal/buildsystem"
	"github.com/wailsapp/wails/v3/internal/flags"
	"github.com/wailsapp/wails/v3/internal/term"
)

func executeBuildPipeline(buildFlags *flags.Build, otherArgs []string, step string) error {
	target, arch := targetFromArgs(otherArgs)
	plan, err := buildsystem.Resolve(buildsystem.Request{
		ConfigPath: buildFlags.Config,
		Target:     target,
		Arch:       arch,
		Mode:       "production",
		Tags:       strings.Split(buildFlags.Tags, ","),
		Obfuscated: buildFlags.Obfuscated,
	})
	if err != nil {
		return err
	}
	if err := resolveBuildActions(plan, buildFlags); err != nil {
		return err
	}

	term.Header("Build Pipeline")
	DisableFooter = true
	return buildsystem.Execute(context.Background(), plan, buildsystem.ExecuteOptions{
		From:            buildFlags.From,
		Until:           buildFlags.Until,
		Step:            step,
		Stdout:          os.Stdout,
		Stderr:          os.Stderr,
		InternalActions: buildInternalActions(buildFlags),
		OnStage: func(stage buildsystem.Stage, status string) {
			switch status {
			case "running":
				term.Infof("%s", stage.ID)
			case "completed":
				term.Success(stage.ID)
			case "skipped":
				term.Infof("%s (skipped: %s)", stage.ID, stage.Reason)
			}
		},
	})
}

func resolveBuildActions(plan *buildsystem.Plan, buildFlags *flags.Build) error {
	for index := range plan.Stages {
		stage := &plan.Stages[index]
		if stage.Status == "skipped" || stage.Replacement != nil {
			continue
		}

		switch stage.ID {
		case "project.resolve":
			stage.Actions = []buildsystem.Action{{
				Kind:        buildsystem.ActionMkdir,
				Description: "Create the generated build workspace",
				Status:      "planned",
				Path:        filepath.Join(plan.Project.Root, ".wails", "build"),
			}}
		case "toolchain.check":
			stage.Actions = toolchainActions(plan)
		case "dependencies.prepare":
			stage.Actions = dependencyActions(plan)
		case "bindings.generate":
			output, err := planStageOutputPath(plan, *stage, "bindings")
			if err != nil {
				return err
			}
			stage.Actions = []buildsystem.Action{internalBuildActionWithParameters(
				"bindings.generate",
				"Generate frontend bindings from the application Go packages",
				map[string]string{
					"buildFlags": "-tags " + strings.Join(plan.Target.Tags, ","),
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
			stage.Actions = []buildsystem.Action{{
				Kind:             buildsystem.ActionCommand,
				Description:      "Build the production frontend distribution",
				Status:           "planned",
				Command:          []string{plan.Project.PackageManager, "run", "build"},
				WorkingDirectory: filepath.Join(plan.Project.Root, plan.Project.Frontend),
				Environment:      map[string]string{"PRODUCTION": "true"},
			}}
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
		case "artifacts.collect":
			binary, err := planArtifactPath(plan, "binary")
			if err != nil {
				return err
			}
			manifest, err := planStageOutputPath(plan, *stage, "manifest")
			if err != nil {
				return err
			}
			stage.Actions = []buildsystem.Action{internalBuildActionWithParameters(
				"artifacts.collect",
				"Hash the native binary and write the artifact manifest",
				map[string]string{
					"binary":   binary,
					"manifest": manifest,
				},
			)}
		}
	}
	addActionContextEnvironment(plan)
	return nil
}

func toolchainActions(plan *buildsystem.Plan) []buildsystem.Action {
	tools := []string{"go"}
	if fileExists(filepath.Join(plan.Project.Root, plan.Project.Frontend, "package.json")) {
		tools = append(tools, plan.Project.PackageManager)
	}
	if plan.Target.Platform == "linux" || plan.Target.Platform == "darwin" {
		tools = append(tools, "cc")
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
	if plan.Target.Platform == "linux" && runtime.GOOS == "linux" {
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

func assetActions(plan *buildsystem.Plan, stage buildsystem.Stage) ([]buildsystem.Action, error) {
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
	switch plan.Target.Platform {
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
	switch plan.Target.Platform {
	case "windows":
		assets, err := planArtifactPath(plan, "assets")
		if err != nil {
			return nil, err
		}
		return append(actions, internalBuildActionWithParameters(
			"platform.generate-syso",
			"Generate the Windows resource object",
			map[string]string{
				"arch":     plan.Target.Arch,
				"icon":     filepath.Join(assets, "icon.ico"),
				"info":     filepath.Join(plan.Project.Root, "build", "windows", "info.json"),
				"manifest": filepath.Join(plan.Project.Root, "build", "windows", "wails.exe.manifest"),
				"output":   filepath.Join(output, "rsrc_windows_"+plan.Target.Arch+".syso"),
			},
		)), nil
	case "darwin":
		assets, err := planArtifactPath(plan, "assets")
		if err != nil {
			return nil, err
		}
		return append(actions,
			buildsystem.Action{
				Kind:        buildsystem.ActionCopy,
				Description: "Copy the macOS application property list",
				Status:      "planned",
				Source:      filepath.Join(plan.Project.Root, "build", "darwin", "Info.plist"),
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
			strings.ReplaceAll(stage.ID, ".", "-")+".json",
		)
		for actionIndex := range stage.Actions {
			action := &stage.Actions[actionIndex]
			if action.Kind != buildsystem.ActionCommand {
				continue
			}
			if action.Environment == nil {
				action.Environment = make(map[string]string)
			}
			action.Environment["WAILS_BUILD_STAGE"] = stage.ID
			action.Environment["WAILS_BUILD_CONTEXT"] = contextPath
		}
	}
}

func dependencyActions(plan *buildsystem.Plan) []buildsystem.Action {
	actions := []buildsystem.Action{{
		Kind:             buildsystem.ActionCommand,
		Description:      "Download Go module dependencies",
		Status:           "planned",
		Command:          []string{"go", "mod", "download"},
		WorkingDirectory: plan.Project.Root,
	}}

	frontend := filepath.Join(plan.Project.Root, plan.Project.Frontend)
	if _, err := os.Stat(filepath.Join(frontend, "package.json")); os.IsNotExist(err) {
		return actions
	}
	name, args := frontendInstallCommand(frontend, plan.Project.PackageManager)
	action := buildsystem.Action{
		Kind:             buildsystem.ActionCommand,
		Description:      "Install frontend dependencies",
		Status:           "planned",
		Command:          append([]string{name}, args...),
		WorkingDirectory: frontend,
	}
	if frontendDependenciesExist(frontend) {
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
	if buildFlags.Obfuscated {
		return []buildsystem.Action{internalBuildAction(
			"native.compile",
			"Compile the obfuscated native application",
		)}, nil
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

	var temporaryResource string
	if plan.Target.Platform == "windows" {
		platform, err := planArtifactPath(plan, "platform")
		if err != nil {
			return nil, err
		}
		name := "rsrc_windows_" + plan.Target.Arch + ".syso"
		temporaryResource = filepath.Join(plan.Project.Root, name)
		actions = append(actions, buildsystem.Action{
			Kind:        buildsystem.ActionCopy,
			Description: "Install the generated Windows resource for Go compilation",
			Status:      "planned",
			Source:      filepath.Join(platform, name),
			Destination: temporaryResource,
		})
	}

	command := []string{"go", "build"}
	if len(plan.Target.Tags) > 0 {
		command = append(command, "-tags", strings.Join(plan.Target.Tags, ","))
	}
	command = append(command, "-trimpath", "-buildvcs=false", "-ldflags=-w -s", "-o", output)
	environment := map[string]string{
		"GOOS":   plan.Target.Platform,
		"GOARCH": plan.Target.Arch,
	}
	if slices.Contains([]string{"linux", "darwin"}, plan.Target.Platform) {
		environment["CGO_ENABLED"] = "1"
	} else {
		environment["CGO_ENABLED"] = "0"
	}
	if plan.Target.Platform == "darwin" {
		environment["CGO_CFLAGS"] = "-mmacosx-version-min=12.0"
		environment["CGO_LDFLAGS"] = "-mmacosx-version-min=12.0"
		environment["MACOSX_DEPLOYMENT_TARGET"] = "12.0"
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

func planArtifactPath(plan *buildsystem.Plan, name string) (string, error) {
	for _, stage := range plan.Stages {
		for _, artifact := range stage.Outputs {
			if artifact.Name == name {
				return absoluteArtifactPath(plan.Project.Root, artifact), nil
			}
		}
	}
	return "", fmt.Errorf("build plan has no artifact %q", name)
}

func buildInternalActions(_ *flags.Build) map[string]buildsystem.ActionFunc {
	return map[string]buildsystem.ActionFunc{
		"bindings.generate":      generateBindingsAction,
		"assets.generate-icons":  generateIconsAction,
		"platform.generate-syso": generateSysoAction,
		"native.compile": func(
			context.Context,
			*buildsystem.StageContext,
			buildsystem.Action,
		) error {
			return fmt.Errorf("obfuscated builds are not yet supported by the typed executor")
		},
		"artifacts.collect": collectArtifactsAction,
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
	return GenerateBindings(options, []string{"."})
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
	Type     string `json:"type"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

func collectArtifactsAction(
	_ context.Context,
	stage *buildsystem.StageContext,
	action buildsystem.Action,
) error {
	binaryArtifact, ok := stage.Artifacts["binary"]
	if !ok {
		return fmt.Errorf("artifact %q is not available to stage %s", "binary", stage.Stage.ID)
	}
	binary := action.Parameters["binary"]
	info, err := os.Stat(binary)
	if err != nil {
		return err
	}
	file, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	manifestPath := action.Parameters["manifest"]
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		return err
	}
	payload := buildArtifactManifest{Artifacts: []buildArtifactEntry{{
		Type:     "binary",
		Platform: stage.Plan.Target.Platform,
		Arch:     stage.Plan.Target.Arch,
		Path:     binaryArtifact.Path,
		Size:     info.Size(),
		SHA256:   hex.EncodeToString(hash.Sum(nil)),
	}}}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, append(data, '\n'), 0o644)
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
