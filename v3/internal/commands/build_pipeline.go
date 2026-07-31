package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

	term.Header("Build Pipeline")
	DisableFooter = true
	return buildsystem.Execute(context.Background(), plan, buildsystem.ExecuteOptions{
		From:     buildFlags.From,
		Until:    buildFlags.Until,
		Step:     step,
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
		Builtins: buildStageImplementations(buildFlags),
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

func buildStageImplementations(buildFlags *flags.Build) map[string]buildsystem.StageFunc {
	return map[string]buildsystem.StageFunc{
		"project.resolve":      resolveProjectStage,
		"toolchain.check":      checkToolchainStage,
		"dependencies.prepare": prepareDependenciesStage,
		"bindings.generate":    generateBindingsStage(buildFlags),
		"assets.generate":      generateAssetsStage,
		"frontend.build":       buildFrontendStage,
		"platform.generate":    generatePlatformStage,
		"native.compile":       compileNativeStage(buildFlags),
		"artifacts.collect":    collectArtifactsStage,
	}
}

func resolveProjectStage(_ context.Context, stage *buildsystem.StageContext) error {
	return os.MkdirAll(filepath.Join(stage.Plan.Project.Root, ".wails", "build"), 0o755)
}

func checkToolchainStage(ctx context.Context, stage *buildsystem.StageContext) error {
	required := []string{"go"}
	if _, err := os.Stat(filepath.Join(stage.Plan.Project.Root, stage.Plan.Project.Frontend, "package.json")); err == nil {
		required = append(required, stage.Plan.Project.PackageManager)
	}
	if stage.Plan.Target.Platform == "linux" || stage.Plan.Target.Platform == "darwin" {
		required = append(required, "cc")
	}
	for _, tool := range required {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("required tool %q was not found in PATH", tool)
		}
	}
	if stage.Plan.Target.Platform == "linux" && runtime.GOOS == "linux" {
		if err := runPipelineCommand(ctx, stage, nil, "pkg-config", "--exists", "gtk4", "webkitgtk-6.0"); err != nil {
			return fmt.Errorf("GTK4 and WebKitGTK 6.0 development packages are required: %w", err)
		}
	}
	return nil
}

func prepareDependenciesStage(ctx context.Context, stage *buildsystem.StageContext) error {
	if err := runPipelineCommand(ctx, stage, nil, "go", "mod", "download"); err != nil {
		return err
	}

	frontend := filepath.Join(stage.Plan.Project.Root, stage.Plan.Project.Frontend)
	if _, err := os.Stat(filepath.Join(frontend, "package.json")); os.IsNotExist(err) {
		return nil
	}
	if frontendDependenciesExist(frontend) {
		return nil
	}
	name, args := frontendInstallCommand(frontend, stage.Plan.Project.PackageManager)
	return runPipelineCommand(ctx, stage, nil, name, args...)
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

func generateBindingsStage(buildFlags *flags.Build) buildsystem.StageFunc {
	return func(_ context.Context, stage *buildsystem.StageContext) error {
		output, err := stageArtifactPath(stage, "bindings")
		if err != nil {
			return err
		}
		tags := strings.Join(stage.Plan.Target.Tags, ",")
		options := &flags.GenerateBindingsOptions{
			BuildFlagsString: "-tags " + tags,
			OutputDirectory:  output,
			ModelsFilename:   "models",
			IndexFilename:    "index",
			TimeType:         "Date",
			TS:               fileExists(filepath.Join(stage.Plan.Project.Root, stage.Plan.Project.Frontend, "tsconfig.json")),
			Obfuscated:       buildFlags.Obfuscated,
			Clean:            true,
			Silent:           true,
		}
		return GenerateBindings(options, []string{"."})
	}
}

func generateAssetsStage(_ context.Context, stage *buildsystem.StageContext) error {
	output, err := stageArtifactPath(stage, "assets")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return err
	}
	input := filepath.Join(stage.Plan.Project.Root, "build", "appicon.png")
	if !fileExists(input) {
		return fmt.Errorf("application icon not found at %s", input)
	}
	switch stage.Plan.Target.Platform {
	case "windows":
		return GenerateIcons(&IconsOptions{
			Input:           input,
			WindowsFilename: filepath.Join(output, "icon.ico"),
		})
	case "darwin":
		return GenerateIcons(&IconsOptions{
			Input:       input,
			MacFilename: filepath.Join(output, "icons.icns"),
		})
	default:
		return copyPipelineFile(input, filepath.Join(output, "appicon.png"))
	}
}

func buildFrontendStage(ctx context.Context, stage *buildsystem.StageContext) error {
	frontend := filepath.Join(stage.Plan.Project.Root, stage.Plan.Project.Frontend)
	if _, err := os.Stat(filepath.Join(frontend, "package.json")); os.IsNotExist(err) {
		return fmt.Errorf("frontend package.json not found in %s", frontend)
	}
	return runPipelineCommand(ctx, stage, []string{"PRODUCTION=true"}, stage.Plan.Project.PackageManager, "run", "build")
}

func generatePlatformStage(_ context.Context, stage *buildsystem.StageContext) error {
	output, err := stageArtifactPath(stage, "platform")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return err
	}
	switch stage.Plan.Target.Platform {
	case "windows":
		assets, err := artifactPath(stage, "assets")
		if err != nil {
			return err
		}
		return GenerateSyso(&SysoOptions{
			Manifest: filepath.Join(stage.Plan.Project.Root, "build", "windows", "wails.exe.manifest"),
			Info:     filepath.Join(stage.Plan.Project.Root, "build", "windows", "info.json"),
			Icon:     filepath.Join(assets, "icon.ico"),
			Out:      filepath.Join(output, "rsrc_windows_"+stage.Plan.Target.Arch+".syso"),
			Arch:     stage.Plan.Target.Arch,
		})
	case "darwin":
		assets, err := artifactPath(stage, "assets")
		if err != nil {
			return err
		}
		if err := copyPipelineFile(
			filepath.Join(stage.Plan.Project.Root, "build", "darwin", "Info.plist"),
			filepath.Join(output, "Info.plist"),
		); err != nil {
			return err
		}
		return copyPipelineFile(filepath.Join(assets, "icons.icns"), filepath.Join(output, "icons.icns"))
	default:
		return nil
	}
}

func compileNativeStage(buildFlags *flags.Build) buildsystem.StageFunc {
	return func(ctx context.Context, stage *buildsystem.StageContext) error {
		if buildFlags.Obfuscated {
			return fmt.Errorf("obfuscated builds are not yet supported by the typed executor")
		}
		output, err := stageArtifactPath(stage, "binary")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
			return err
		}

		var cleanup func()
		if stage.Plan.Target.Platform == "windows" {
			platform, err := artifactPath(stage, "platform")
			if err != nil {
				return err
			}
			name := "rsrc_windows_" + stage.Plan.Target.Arch + ".syso"
			temporary := filepath.Join(stage.Plan.Project.Root, name)
			if err := copyPipelineFile(filepath.Join(platform, name), temporary); err != nil {
				return err
			}
			cleanup = func() { _ = os.Remove(temporary) }
			defer cleanup()
		}

		args := []string{"build"}
		if len(stage.Plan.Target.Tags) > 0 {
			args = append(args, "-tags", strings.Join(stage.Plan.Target.Tags, ","))
		}
		args = append(args, "-trimpath", "-buildvcs=false", "-ldflags=-w -s", "-o", output)
		environment := []string{
			"GOOS=" + stage.Plan.Target.Platform,
			"GOARCH=" + stage.Plan.Target.Arch,
		}
		if slices.Contains([]string{"linux", "darwin"}, stage.Plan.Target.Platform) {
			environment = append(environment, "CGO_ENABLED=1")
		} else {
			environment = append(environment, "CGO_ENABLED=0")
		}
		if stage.Plan.Target.Platform == "darwin" {
			environment = append(environment,
				"CGO_CFLAGS=-mmacosx-version-min=12.0",
				"CGO_LDFLAGS=-mmacosx-version-min=12.0",
				"MACOSX_DEPLOYMENT_TARGET=12.0",
			)
		}
		return runPipelineCommand(ctx, stage, environment, "go", args...)
	}
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

func collectArtifactsStage(_ context.Context, stage *buildsystem.StageContext) error {
	binaryArtifact, ok := stage.Artifacts["binary"]
	if !ok {
		return fmt.Errorf("artifact %q is not available to stage %s", "binary", stage.Stage.ID)
	}
	binary := absoluteArtifactPath(stage.Plan.Project.Root, binaryArtifact)
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
	manifestPath, err := stageArtifactPath(stage, "manifest")
	if err != nil {
		return err
	}
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

func runPipelineCommand(
	ctx context.Context,
	stage *buildsystem.StageContext,
	environment []string,
	name string,
	args ...string,
) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = stage.Plan.Project.Root
	if name == stage.Plan.Project.PackageManager {
		command.Dir = filepath.Join(stage.Plan.Project.Root, stage.Plan.Project.Frontend)
	}
	command.Env = append(os.Environ(), environment...)
	command.Stdout = stage.Stdout
	command.Stderr = stage.Stderr
	command.Stdin = os.Stdin
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(append([]string{name}, args...), " "), err)
	}
	return nil
}

func stageArtifactPath(stage *buildsystem.StageContext, name string) (string, error) {
	for _, artifact := range stage.Stage.Outputs {
		if artifact.Name == name {
			return absoluteArtifactPath(stage.Plan.Project.Root, artifact), nil
		}
	}
	return "", fmt.Errorf("stage %s has no output artifact %q", stage.Stage.ID, name)
}

func artifactPath(stage *buildsystem.StageContext, name string) (string, error) {
	artifact, ok := stage.Artifacts[name]
	if !ok {
		return "", fmt.Errorf("artifact %q is not available to stage %s", name, stage.Stage.ID)
	}
	return absoluteArtifactPath(stage.Plan.Project.Root, artifact), nil
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

func copyPipelineFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}
