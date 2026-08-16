package commands

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/wailsapp/wails/v3/internal/buildsystem"
	"github.com/wailsapp/wails/v3/internal/buildwarnings"
	"github.com/wailsapp/wails/v3/internal/flags"
	"github.com/wailsapp/wails/v3/internal/term"
	"github.com/wailsapp/wails/v3/internal/wake"
)

// runTaskFunc is a variable to allow mocking in tests
var runTaskFunc = RunTask

// validPlatforms for GOOS
var validPlatforms = map[string]bool{
	"windows": true,
	"darwin":  true,
	"linux":   true,
}

// rootDispatchTasks are the verbs that this wrapper routes through the
// top-level task in the generated root Taskfile, which dispatches to the
// platform-specific task via the GOOS variable (e.g. `build` -> `{{.GOOS}}:build`).
// For these we run the root task and pass GOOS as a variable, so user
// customisations in the root Taskfile are honoured for both native and
// cross-compilation builds. Only `build` and `package` go through this wrapper:
// the root Taskfile also defines a `run` dispatch task, but `run` is invoked by
// `wails3 dev` directly rather than through here. Verbs without a root task
// (e.g. `sign`, which only exists per-platform) always target the
// platform-specific task directly.
var rootDispatchTasks = map[string]bool{
	"build":   true,
	"package": true,
}

const (
	// mcpEnvVar enables the MCP service build tag when set to a truthy value.
	mcpEnvVar = "WAILS_MCP"
	// mcpBuildTag is the Go build tag that compiles in the MCP service.
	mcpBuildTag = "mcp"
)

// envTags returns the build tags implied by environment variables.
func envTags() []string {
	var tags []string
	switch strings.ToLower(strings.TrimSpace(os.Getenv(mcpEnvVar))) {
	case "1", "true", "on", "yes":
		tags = append(tags, mcpBuildTag)
	}
	return tags
}

// mergeTags appends extra tags to a comma-separated tag list, skipping duplicates.
func mergeTags(tags string, extra ...string) string {
	existing := strings.Split(tags, ",")
	for _, tag := range extra {
		if !slices.Contains(existing, tag) {
			existing = append(existing, tag)
		}
	}
	return strings.Trim(strings.Join(existing, ","), ",")
}

func Build(buildFlags *flags.Build, otherArgs []string) error {
	buildFlags.Tags = mergeTags(buildFlags.Tags, envTags()...)
	step, pipelineArgs, err := buildStepInvocation(otherArgs)
	if err != nil {
		return err
	}
	if buildFlags.JSON && !buildFlags.Plan {
		return fmt.Errorf("--json requires --plan")
	}
	if buildFlags.Plan {
		return printBuildPlan(buildFlags, pipelineArgs, step)
	}
	explicitPipeline, err := buildsystem.UsesExplicitPipeline(buildFlags.Config)
	if err != nil {
		return err
	}
	if buildFlags.Pipeline || explicitPipeline || buildFlags.From != "" || buildFlags.Until != "" || step != "" ||
		buildFlags.NoCache || buildFlags.Resume || buildFlags.CacheDir != "" || buildFlags.Report != "" {
		return executeBuildPipeline(buildFlags, pipelineArgs, step)
	}
	if buildFlags.Tags != "" {
		otherArgs = append(otherArgs, "EXTRA_TAGS="+buildFlags.Tags)
	}
	if buildFlags.Obfuscated {
		otherArgs = append(otherArgs, "OBFUSCATED=true")
	}
	if buildFlags.GarbleArgs != "" {
		otherArgs = append(otherArgs, "GARBLE_ARGS="+buildFlags.GarbleArgs)
	}
	return wrapTask("build", otherArgs)
}

func buildStepInvocation(args []string) (string, []string, error) {
	if len(args) == 0 || args[0] != "step" {
		return "", args, nil
	}
	if len(args) < 2 || strings.Contains(args[1], "=") {
		return "", nil, fmt.Errorf("usage: wails3 build step <stage> [GOOS=...] [GOARCH=...]")
	}
	return args[1], args[2:], nil
}

func printBuildPlan(buildFlags *flags.Build, otherArgs []string, step string) error {
	return printTypedPipelinePlan(buildFlags, nil, "build", otherArgs, step)
}

func printTypedPipelinePlan(
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
	DisableFooter = true
	if step != "" {
		inspection, err := buildsystem.InspectStage(plan, step)
		if err != nil {
			return err
		}
		if buildFlags.JSON {
			return buildsystem.WriteStageJSON(os.Stdout, inspection)
		}
		return buildsystem.WriteStageText(os.Stdout, inspection)
	}
	if buildFlags.JSON {
		return buildsystem.WriteJSON(os.Stdout, plan)
	}
	return buildsystem.WriteText(os.Stdout, plan)
}

func buildModeFromArgs(args []string) string {
	for _, arg := range args {
		if strings.EqualFold(arg, "DEV=true") || strings.EqualFold(arg, "DEV=1") {
			return "development"
		}
	}
	return "production"
}

func requestedTargets(values []string) ([]buildsystem.Target, error) {
	result := make([]buildsystem.Target, 0, len(values))
	for _, value := range values {
		parts := strings.Split(value, "/")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("invalid build target %q; expected platform/architecture", value)
		}
		result = append(result, buildsystem.Target{
			Platform: strings.TrimSpace(parts[0]),
			Arch:     strings.TrimSpace(parts[1]),
		})
	}
	return result, nil
}

func targetFromArgs(args []string) (string, string) {
	target := os.Getenv("GOOS")
	arch := os.Getenv("GOARCH")
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "GOOS="):
			target = strings.TrimPrefix(arg, "GOOS=")
		case strings.HasPrefix(arg, "GOARCH="):
			arch = strings.TrimPrefix(arg, "GOARCH=")
		case strings.HasPrefix(arg, "ARCH="):
			arch = strings.TrimPrefix(arg, "ARCH=")
		}
	}
	return target, arch
}

func Package(options *flags.Package, otherArgs []string) error {
	if options.JSON && !options.Plan {
		return fmt.Errorf("--json requires --plan")
	}
	explicitPipeline, err := buildsystem.UsesExplicitPipeline(options.Config)
	if err != nil {
		return err
	}
	if packagePipelineRequested(options) || explicitPipeline {
		buildFlags := packageBuildFlags(options)
		if options.Plan {
			return printTypedPipelinePlan(buildFlags, nil, "package", otherArgs, "")
		}
		return executeTypedPipeline(buildFlags, nil, "package", otherArgs, "")
	}
	return wrapTask("package", otherArgs)
}

func SignWrapper(options *flags.SignWrapper, otherArgs []string) error {
	if options.JSON && !options.Plan {
		return fmt.Errorf("--json requires --plan")
	}
	explicitPipeline, err := buildsystem.UsesExplicitPipeline(options.Config)
	if err != nil {
		return err
	}
	if signingPipelineRequested(options) || explicitPipeline {
		buildFlags := signBuildFlags(options)
		signOptions := pipelineSignOptions(options)
		goal := "sign"
		if options.Notarize {
			goal = "notarize"
		}
		if options.Plan {
			return printTypedPipelinePlan(buildFlags, signOptions, goal, otherArgs, "")
		}
		return executeTypedPipeline(buildFlags, signOptions, goal, otherArgs, "")
	}
	return wrapTask("sign", otherArgs)
}

func packagePipelineRequested(options *flags.Package) bool {
	return options.Plan || options.Pipeline || options.Parallel ||
		options.NoCache || options.Resume || options.CacheDir != "" || options.Report != "" ||
		len(options.Targets) > 0 || len(options.Formats) > 0
}

func signingPipelineRequested(options *flags.SignWrapper) bool {
	return options.Plan || options.Pipeline || options.Parallel || options.Notarize ||
		options.NoCache || options.Resume || options.CacheDir != "" || options.Report != "" ||
		len(options.Targets) > 0 || len(options.Formats) > 0 ||
		options.Certificate != "" || options.Thumbprint != "" || options.Timestamp != "" ||
		options.Identity != "" || options.Entitlements != "" || options.HardenedRuntime ||
		options.KeychainProfile != "" || options.PGPKey != "" || options.Role != ""
}

func packageBuildFlags(options *flags.Package) *flags.Build {
	return &flags.Build{
		Config:   options.Config,
		Targets:  options.Targets,
		Packages: options.Formats,
		JSON:     options.JSON,
		Plan:     options.Plan,
		Pipeline: options.Pipeline,
		Parallel: options.Parallel,
		NoCache:  options.NoCache,
		Resume:   options.Resume,
		CacheDir: options.CacheDir,
		Report:   options.Report,
	}
}

func signBuildFlags(options *flags.SignWrapper) *flags.Build {
	return &flags.Build{
		Config:   options.Config,
		Targets:  options.Targets,
		Packages: options.Formats,
		JSON:     options.JSON,
		Plan:     options.Plan,
		Pipeline: options.Pipeline,
		Parallel: options.Parallel,
		NoCache:  options.NoCache,
		Resume:   options.Resume,
		CacheDir: options.CacheDir,
		Report:   options.Report,
	}
}

func pipelineSignOptions(options *flags.SignWrapper) *flags.Sign {
	return &flags.Sign{
		Certificate:     options.Certificate,
		Thumbprint:      options.Thumbprint,
		Timestamp:       options.Timestamp,
		Identity:        options.Identity,
		Entitlements:    options.Entitlements,
		HardenedRuntime: options.HardenedRuntime,
		Notarize:        options.Notarize,
		KeychainProfile: options.KeychainProfile,
		PGPKey:          options.PGPKey,
		Role:            options.Role,
	}
}

func wrapTask(action string, otherArgs []string) error {
	// Match the banner other wails3 commands print; the footer is restored by
	// leaving DisableFooter at its default so printFooter runs on exit.
	term.Header(title(action))

	// Create a per-invocation warnings file so subprocess commands (e.g.
	// wails3 tool has-cc) can append deprecation notices that we collect
	// and print after the task finishes.
	if f, err := os.CreateTemp("", "wails-build-warnings-*"); err == nil {
		f.Close()
		prev, hadPrev := os.LookupEnv(buildwarnings.EnvVar)
		os.Setenv(buildwarnings.EnvVar, f.Name())
		defer func() {
			buildwarnings.FlushAndPrint()
			if hadPrev {
				os.Setenv(buildwarnings.EnvVar, prev)
			} else {
				os.Unsetenv(buildwarnings.EnvVar)
			}
		}()
	}

	goos := os.Getenv("GOOS")
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := os.Getenv("GOARCH")
	if goarch == "" {
		goarch = runtime.GOARCH
	}

	var remainingArgs []string

	for _, arg := range otherArgs {
		switch {
		case strings.HasPrefix(arg, "GOOS="):
			goos = strings.TrimPrefix(arg, "GOOS=")
		case strings.HasPrefix(arg, "GOARCH="):
			goarch = strings.TrimPrefix(arg, "GOARCH=")
		default:
			remainingArgs = append(remainingArgs, arg)
		}
	}

	// platformTaskName always targets the platform-specific task (e.g.
	// "linux:build"). The experimental wake runner is built against this concrete
	// name, so it keeps using it unconditionally.
	platformTaskName := action
	if validPlatforms[goos] {
		platformTaskName = goos + ":" + action
	}

	// Pass GOOS/ARCH through as Taskfile variables. The root build/package/run
	// tasks dispatch on {{.GOOS}}, so running the root task (rather than the
	// platform-prefixed one) means any customisations in the root Taskfile are
	// honoured, for both native and cross-compilation builds. Verbs without a
	// root task (e.g. `sign`) still target the platform task directly. See #5615.
	remainingArgs = append(remainingArgs, "GOOS="+goos, "ARCH="+goarch)

	taskName := platformTaskName
	if rootDispatchTasks[action] {
		taskName = action
	}

	if useWake() {
		return runWakeTask(action, platformTaskName, goos, goarch, remainingArgs)
	}

	newArgs := []string{"wails3", "task", taskName}
	newArgs = append(newArgs, remainingArgs...)
	os.Args = newArgs
	return runTaskFunc(&RunTaskOptions{Name: taskName}, remainingArgs)
}

func useWake() bool {
	return os.Getenv("WAILS_USE_WAKE") == "true"
}

// title capitalises an action ("build" -> "Build") for the command banner.
func title(action string) string {
	if action == "" {
		return action
	}
	return strings.ToUpper(action[:1]) + action[1:]
}

func runWakeTask(verb, taskName, goos, goarch string, cliVars []string) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}

	vars := make(map[string]string)
	for _, v := range cliVars {
		if strings.Contains(v, "=") {
			parts := strings.SplitN(v, "=", 2)
			if len(parts) == 2 {
				vars[parts[0]] = parts[1]
			}
		}
	}

	opts := wake.ExecuteOptions{
		Dir:      dir,
		Platform: goos,
		Arch:     goarch,
		Verb:     verb,
		Vars:     vars,
		Verbose:  os.Getenv("WAKE_VERBOSE") != "",
		Silent:   os.Getenv("WAKE_SILENT") != "",
		Debug:    os.Getenv("WAKE_DEBUG") != "",
		// Parallel execution is the default. Set WAKE_SERIAL=true to opt out
		// (useful when debugging task ordering or when stdout interleaving
		// from sibling steps would muddle a specific investigation).
		Parallel: os.Getenv("WAKE_SERIAL") == "",
		// WAKE_FORCE=true skips every cache lookup, both the Taskfile
		// sources/generates/status check and the implicit native-Go cache.
		// Use when you want a true "clean" build without rm -rf .wake/.
		Force: os.Getenv("WAKE_FORCE") != "",
	}

	return wake.Execute(taskName, opts)
}
