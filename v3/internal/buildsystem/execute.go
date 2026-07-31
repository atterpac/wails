package buildsystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type StageFunc func(context.Context, *StageContext) error
type ActionFunc func(context.Context, *StageContext, Action) error

type ExecuteOptions struct {
	From            string
	Until           string
	Step            string
	Parallel        bool
	DisableCache    bool
	Resume          bool
	CacheDirectory  string
	ReportPath      string
	Stdout          io.Writer
	Stderr          io.Writer
	Builtins        map[string]StageFunc
	InternalActions map[string]ActionFunc
	OnStage         func(Stage, string)
	OnAction        func(Stage, Action, string)
	callbackMu      *sync.Mutex
	hookRuns        *hookRunState
	runtime         *executionRuntime
}

type StageContext struct {
	Plan       *Plan
	Stage      Stage
	Artifacts  map[string]Artifact
	Stdout     io.Writer
	Stderr     io.Writer
	HookInputs map[string]string
}

type hookRunState struct {
	mu      sync.Mutex
	entries map[string]*hookRun
}

type hookRun struct {
	done chan struct{}
	err  error
}

type synchronizedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (writer *synchronizedWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.writer.Write(data)
}

func Execute(ctx context.Context, plan *Plan, options ExecuteOptions) (resultErr error) {
	if plan == nil {
		return errors.New("build plan is nil")
	}
	if err := Validate(plan); err != nil {
		return fmt.Errorf("validate build plan: %w", err)
	}
	if options.Stdout == nil {
		options.Stdout = os.Stdout
	}
	if options.Stderr == nil {
		options.Stderr = os.Stderr
	}
	if options.callbackMu == nil {
		options.callbackMu = &sync.Mutex{}
	}
	if options.hookRuns == nil {
		options.hookRuns = &hookRunState{entries: make(map[string]*hookRun)}
	}
	if options.Parallel {
		options.Stdout = &synchronizedWriter{writer: options.Stdout}
		options.Stderr = &synchronizedWriter{writer: options.Stderr}
	}
	order, selected, err := executionOrder(plan.Stages, options)
	if err != nil {
		return err
	}
	runtime, err := newExecutionRuntime(plan, options, selected)
	if err != nil {
		return err
	}
	options.runtime = runtime
	if runtime.report != nil {
		defer func() {
			if reportErr := runtime.report.finish(resultErr); reportErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("write execution report: %w", reportErr))
			}
		}()
	}
	artifacts := collectArtifacts(prerequisiteStages(plan.Stages, selected))
	if options.Parallel {
		return executeParallel(ctx, plan, options, selected, artifacts)
	}
	return executeSequential(ctx, plan, options, order, artifacts)
}

func executeSequential(
	ctx context.Context,
	plan *Plan,
	options ExecuteOptions,
	order []int,
	artifacts map[string]Artifact,
) error {
	for _, index := range order {
		stage := plan.Stages[index]
		outputs, err := executeStage(ctx, plan, stage, options, artifacts)
		if err != nil {
			return err
		}
		registerArtifacts(artifacts, outputs)
	}
	return nil
}

func executeStage(
	ctx context.Context,
	plan *Plan,
	stage Stage,
	options ExecuteOptions,
	artifacts map[string]Artifact,
) (outputs []Artifact, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cacheDecision := "disabled"
	fingerprint := ""
	if stage.Cache.Enabled && options.runtime != nil && options.runtime.cache != nil {
		var err error
		fingerprint, err = options.runtime.cache.fingerprint(plan, stage, artifacts)
		if err != nil {
			return nil, fmt.Errorf("fingerprint stage %s: %w", stage.Reference(), err)
		}
		if options.runtime.cacheEnabled {
			cacheDecision = "miss"
		}
	}
	if options.runtime != nil && options.runtime.report != nil {
		options.runtime.report.stageStarted(stage, fingerprint, cacheDecision)
		defer func() {
			status := "completed"
			if resultErr != nil {
				status = "failed"
			}
			options.runtime.report.stageFinished(stage, status, cacheDecision, resultErr)
		}()
	}
	if stage.Status == "skipped" {
		cacheDecision = "not-applicable"
		notifyStage(options, stage, "skipped")
		return nil, nil
	}
	if fingerprint != "" && options.runtime.canResume(stage, fingerprint) {
		cacheDecision = "resumed"
		notifyStage(options, stage, "resumed")
		return stage.Outputs, nil
	}
	if fingerprint != "" && options.runtime.cacheEnabled {
		restored, err := options.runtime.cache.restore(stage, fingerprint)
		if err != nil {
			fmt.Fprintf(options.Stderr, "cache restore for %s failed; executing stage: %v\n", stage.Reference(), err)
		} else if restored {
			if err := verifyOutputs(plan.Project.Root, stage); err == nil {
				cacheDecision = "hit"
				notifyStage(options, stage, "cached")
				return stage.Outputs, nil
			}
		}
	}

	stageContext := &StageContext{
		Plan:      plan,
		Stage:     stage,
		Artifacts: cloneArtifacts(artifacts),
		Stdout:    options.Stdout,
		Stderr:    options.Stderr,
	}
	if err := verifyInputs(plan.Project.Root, stage); err != nil {
		return nil, err
	}
	notifyStage(options, stage, "running")
	if err := executeHooks(ctx, stageContext, stage.Before, "before", options); err != nil {
		return nil, fmt.Errorf("stage %s before hook: %w", stage.Reference(), err)
	}
	if stage.Replacement != nil {
		if err := executeCommand(
			ctx,
			stageContext,
			appendCommand(stage.Replacement),
			stage.Replacement.WorkingDirectory,
			stage.Replacement.Environment,
			"",
		); err != nil {
			return nil, fmt.Errorf("stage %s replacement: %w", stage.Reference(), err)
		}
	} else if len(stage.Actions) > 0 {
		if err := executeActions(ctx, stageContext, options); err != nil {
			return nil, fmt.Errorf("stage %s: %w", stage.Reference(), err)
		}
	} else {
		implementation := options.Builtins[stage.ID]
		if implementation == nil {
			return nil, fmt.Errorf("stage %s has no built-in implementation", stage.Reference())
		}
		if err := implementation(ctx, stageContext); err != nil {
			return nil, fmt.Errorf("stage %s: %w", stage.Reference(), err)
		}
	}
	registerArtifacts(stageContext.Artifacts, stage.Outputs)
	if err := executeHooks(ctx, stageContext, stage.After, "after", options); err != nil {
		return nil, fmt.Errorf("stage %s after hook: %w", stage.Reference(), err)
	}
	if err := verifyOutputs(plan.Project.Root, stage); err != nil {
		return nil, err
	}
	notifyStage(options, stage, "completed")
	if fingerprint != "" && options.runtime.cacheEnabled {
		if err := options.runtime.cache.store(stage, fingerprint); err != nil {
			cacheDecision = "store-failed"
			fmt.Fprintf(options.Stderr, "cache store for %s failed: %v\n", stage.Reference(), err)
		} else {
			cacheDecision = "stored"
		}
	}
	return stage.Outputs, nil
}

func executeActions(ctx context.Context, stageContext *StageContext, options ExecuteOptions) error {
	actions := stageContext.Stage.Actions
	for index, action := range actions {
		if action.Status == "skipped" {
			notifyAction(options, stageContext.Stage, action, "skipped")
			continue
		}
		notifyAction(options, stageContext.Stage, action, "running")
		if options.runtime != nil && options.runtime.report != nil {
			options.runtime.report.actionStarted(stageContext.Stage, index)
		}
		if err := executeAction(ctx, stageContext, action, options.InternalActions); err != nil {
			if options.runtime != nil && options.runtime.report != nil {
				options.runtime.report.actionFinished(stageContext.Stage, index, "failed", err)
			}
			cleanupErr := executeFinalActions(ctx, stageContext, options, actions[index+1:])
			return errors.Join(err, cleanupErr)
		}
		if options.runtime != nil && options.runtime.report != nil {
			options.runtime.report.actionFinished(stageContext.Stage, index, "completed", nil)
		}
		notifyAction(options, stageContext.Stage, action, "completed")
	}
	return nil
}

func executeFinalActions(
	ctx context.Context,
	stageContext *StageContext,
	options ExecuteOptions,
	actions []Action,
) error {
	ctx = context.WithoutCancel(ctx)
	var result error
	for offset, action := range actions {
		if !action.Finally || action.Status == "skipped" {
			continue
		}
		notifyAction(options, stageContext.Stage, action, "running")
		index := len(stageContext.Stage.Actions) - len(actions) + offset
		if options.runtime != nil && options.runtime.report != nil {
			options.runtime.report.actionStarted(stageContext.Stage, index)
		}
		if err := executeAction(ctx, stageContext, action, options.InternalActions); err != nil {
			if options.runtime != nil && options.runtime.report != nil {
				options.runtime.report.actionFinished(stageContext.Stage, index, "failed", err)
			}
			result = errors.Join(result, err)
			continue
		}
		if options.runtime != nil && options.runtime.report != nil {
			options.runtime.report.actionFinished(stageContext.Stage, index, "completed", nil)
		}
		notifyAction(options, stageContext.Stage, action, "completed")
	}
	return result
}

func executeAction(
	ctx context.Context,
	stageContext *StageContext,
	action Action,
	internalActions map[string]ActionFunc,
) error {
	switch action.Kind {
	case ActionCommand:
		command := action.Command
		if action.Shell {
			command = action.ResolvedCommand
			if len(command) == 0 {
				command = shellCommand(action.Command)
			}
		}
		return executeCommand(
			ctx,
			stageContext,
			command,
			action.WorkingDirectory,
			action.Environment,
			action.Timeout,
		)
	case ActionCheckTool:
		if _, err := exec.LookPath(action.Tool); err != nil {
			return fmt.Errorf("required tool %q was not found in PATH", action.Tool)
		}
		return nil
	case ActionCopy:
		source := expand(action.Source, stageContext)
		if action.Optional {
			if _, err := os.Stat(source); os.IsNotExist(err) {
				return nil
			}
		}
		destination := expand(action.Destination, stageContext)
		if action.Recursive {
			if err := os.RemoveAll(destination); err != nil {
				return err
			}
			return copyPath(source, destination)
		}
		return copyFile(source, destination)
	case ActionInternal:
		implementation := internalActions[action.Internal]
		if implementation == nil {
			return fmt.Errorf("internal action %q has no implementation", action.Internal)
		}
		return implementation(ctx, stageContext, action)
	case ActionMkdir:
		return os.MkdirAll(expand(action.Path, stageContext), 0o755)
	case ActionRemove:
		path := expand(action.Path, stageContext)
		if action.Recursive {
			return os.RemoveAll(path)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case ActionVerify:
		path := expand(action.Path, stageContext)
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("verify %s: %w", path, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported action kind %q", action.Kind)
	}
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
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
	if err := output.Chmod(info.Mode().Perm()); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func executionOrder(stages []Stage, options ExecuteOptions) ([]int, map[string]bool, error) {
	selected := make(map[string]bool, len(stages))
	if len(stages) == 0 {
		return nil, selected, nil
	}
	if options.Step != "" {
		index, err := resolveStageIndex(stages, options.Step)
		if err != nil {
			return nil, nil, err
		}
		selected[stages[index].Reference()] = true
		return []int{index}, selected, nil
	}

	first, last := -1, -1
	if options.From != "" {
		var err error
		first, err = resolveStageIndex(stages, options.From)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve --from: %w", err)
		}
	}
	if options.Until != "" {
		var err error
		last, err = resolveStageIndex(stages, options.Until)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve --until: %w", err)
		}
	}
	fromID, untilID := "", ""
	if first >= 0 {
		fromID = stages[first].Reference()
	}
	if last >= 0 {
		untilID = stages[last].Reference()
	}
	if first >= 0 && last >= 0 && first != last && !stageDependsOn(stages[last], fromID, stages) {
		if first > last {
			return nil, nil, fmt.Errorf("--from stage %q comes after --until stage %q", fromID, untilID)
		}
		return nil, nil, fmt.Errorf(
			"--from stage %q is not a dependency of --until stage %q",
			fromID,
			untilID,
		)
	}

	for _, stage := range stages {
		include := true
		if first >= 0 {
			include = stage.Reference() == fromID || stageDependsOn(stage, fromID, stages)
		}
		if include && last >= 0 {
			include = stage.Reference() == untilID || stageDependsOn(stages[last], stage.Reference(), stages)
		}
		if include {
			selected[stage.Reference()] = true
		}
	}
	return topologicalStageIndexes(stages, selected), selected, nil
}

func topologicalStageIndexes(stages []Stage, selected map[string]bool) []int {
	completed := make(map[string]bool, len(selected))
	result := make([]int, 0, len(selected))
	for len(result) < len(selected) {
		for index, stage := range stages {
			if !selected[stage.Reference()] || completed[stage.Reference()] {
				continue
			}
			ready := true
			for _, dependency := range stage.Needs {
				if selected[dependency] && !completed[dependency] {
					ready = false
					break
				}
			}
			if ready {
				completed[stage.Reference()] = true
				result = append(result, index)
			}
		}
	}
	return result
}

func stageDependsOn(stage Stage, dependencyID string, stages []Stage) bool {
	indexes := make(map[string]int, len(stages))
	for index := range stages {
		indexes[stages[index].Reference()] = index
	}
	return dependsOn(stage.Reference(), dependencyID, stages, indexes, make(map[string]bool))
}

func prerequisiteStages(stages []Stage, selected map[string]bool) []Stage {
	indexes := make(map[string]int, len(stages))
	for index := range stages {
		indexes[stages[index].Reference()] = index
	}
	prerequisites := make(map[string]bool)
	var visit func(string)
	visit = func(id string) {
		for _, dependency := range stages[indexes[id]].Needs {
			if prerequisites[dependency] || selected[dependency] {
				continue
			}
			prerequisites[dependency] = true
			visit(dependency)
		}
	}
	for id := range selected {
		visit(id)
	}

	result := make([]Stage, 0, len(prerequisites))
	for _, stage := range stages {
		if prerequisites[stage.Reference()] {
			result = append(result, stage)
		}
	}
	return result
}

func resolveStageIndex(stages []Stage, selector string) (int, error) {
	for index, stage := range stages {
		if stage.Reference() == selector {
			return index, nil
		}
	}
	match := -1
	var instances []string
	for index, stage := range stages {
		if stage.ID != selector {
			continue
		}
		match = index
		instances = append(instances, stage.Reference())
	}
	if len(instances) == 1 {
		return match, nil
	}
	if len(instances) > 1 {
		return -1, fmt.Errorf(
			"build stage %q is ambiguous; select one of %s",
			selector,
			strings.Join(instances, ", "),
		)
	}
	return -1, fmt.Errorf("unknown build stage %q", selector)
}

func collectArtifacts(stages []Stage) map[string]Artifact {
	result := make(map[string]Artifact)
	for _, stage := range stages {
		registerArtifacts(result, stage.Outputs)
	}
	return result
}

func cloneArtifacts(artifacts map[string]Artifact) map[string]Artifact {
	result := make(map[string]Artifact, len(artifacts))
	for reference, artifact := range artifacts {
		result[reference] = artifact
	}
	return result
}

func verifyInputs(root string, stage Stage) error {
	for _, input := range stage.Inputs {
		if input.Path == "" {
			continue
		}
		if _, err := os.Stat(resolvePath(root, input.Path)); err != nil {
			return fmt.Errorf("stage %s requires artifact %q at %s: %w", stage.Reference(), input.Name, input.Path, err)
		}
	}
	return nil
}

func verifyOutputs(root string, stage Stage) error {
	for _, output := range stage.Outputs {
		if output.Optional {
			continue
		}
		if output.Path == "" {
			continue
		}
		if _, err := os.Stat(resolvePath(root, output.Path)); err != nil {
			return fmt.Errorf("stage %s did not produce artifact %q at %s: %w", stage.Reference(), output.Name, output.Path, err)
		}
	}
	return nil
}

func executeHooks(ctx context.Context, stageContext *StageContext, hooks []Hook, position string, options ExecuteOptions) error {
	for _, hook := range hooks {
		if hook.Status == "skipped" {
			continue
		}
		err := options.hookRuns.execute(ctx, hookScopeKey(stageContext.Stage, hook, position), func() error {
			return executeHook(ctx, stageContext, hook)
		})
		if err != nil {
			name := hook.Name
			if name == "" {
				name = strings.Join(hook.Command, " ")
			}
			if hook.OnFailure == "continue" {
				fmt.Fprintf(stageContext.Stderr, "hook %s failed and was allowed to continue: %v\n", name, err)
				continue
			}
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func (state *hookRunState) execute(ctx context.Context, key string, run func() error) error {
	state.mu.Lock()
	if existing := state.entries[key]; existing != nil {
		state.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-existing.done:
			return existing.err
		}
	}
	entry := &hookRun{done: make(chan struct{})}
	state.entries[key] = entry
	state.mu.Unlock()

	entry.err = run()
	close(entry.done)
	return entry.err
}

func hookScopeKey(stage Stage, hook Hook, position string) string {
	identity := hook.ID
	if identity == "" {
		identity = stage.ID + "." + position + "." + hook.Name + "." + strings.Join(hook.Command, "\x00")
	}
	key := identity + "/" + hook.Scope
	platform, arch, format := "all", "all", "all"
	if stage.Target != nil {
		platform, arch = stage.Target.Platform, stage.Target.Arch
	}
	for _, artifact := range append(stage.Inputs, stage.Outputs...) {
		if artifact.Target != nil && artifact.Target.Format != "" {
			format = artifact.Target.Format
			break
		}
	}
	switch hook.Scope {
	case "build":
		return key
	case "target":
		return key + "/" + platform + "/" + arch
	case "architecture":
		return key + "/" + arch
	case "package":
		return key + "/" + format
	default:
		return key + "/" + stage.Reference()
	}
}

func executeHook(ctx context.Context, stageContext *StageContext, hook Hook) error {
	environment := make(map[string]string, len(hook.Environment)+len(hook.Inputs))
	for name, value := range hook.Environment {
		environment[name] = value
	}
	stageContext.HookInputs = make(map[string]string, len(hook.Inputs))
	defer func() { stageContext.HookInputs = nil }()
	for name, value := range hook.Inputs {
		path := expand(value, stageContext)
		if !filepath.IsAbs(path) {
			path = resolvePath(stageContext.Plan.Project.Root, path)
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("input %q at %s: %w", name, path, err)
		}
		stageContext.HookInputs[name] = path
		environment["WAILS_HOOK_INPUT_"+environmentName(name)] = path
	}
	command := hook.Command
	if hook.Shell {
		command = hook.ResolvedCommand
		if len(command) == 0 {
			command = shellCommand(hook.Command)
		}
	}
	if err := executeCommand(ctx, stageContext, command, hook.WorkingDirectory, environment, hook.Timeout); err != nil {
		return err
	}
	for name, value := range hook.Outputs {
		path := expand(value, stageContext)
		if !filepath.IsAbs(path) {
			path = resolvePath(stageContext.Plan.Project.Root, path)
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("declared output %q at %s: %w", name, path, err)
		}
		for _, output := range stageContext.Stage.Outputs {
			if output.Name == name && resolvePath(stageContext.Plan.Project.Root, output.Path) == path {
				registerArtifacts(stageContext.Artifacts, []Artifact{output})
				break
			}
		}
	}
	return nil
}

func environmentName(value string) string {
	value = strings.ToUpper(value)
	return strings.Map(func(character rune) rune {
		if character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			return character
		}
		return '_'
	}, value)
}

func appendCommand(command *Command) []string {
	result := make([]string, 0, len(command.Command)+len(command.Args))
	result = append(result, command.Command...)
	return append(result, command.Args...)
}

func executeCommand(
	ctx context.Context,
	stageContext *StageContext,
	command []string,
	workingDirectory string,
	environment map[string]string,
	timeout string,
) error {
	if len(command) == 0 {
		return errors.New("empty command")
	}
	if timeout != "" {
		duration, err := time.ParseDuration(timeout)
		if err != nil {
			return fmt.Errorf("invalid timeout %q: %w", timeout, err)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	expanded := make([]string, len(command))
	for index, argument := range command {
		expanded[index] = expand(argument, stageContext)
	}
	cmd := exec.CommandContext(ctx, expanded[0], expanded[1:]...)
	cmd.Dir = stageContext.Plan.Project.Root
	if workingDirectory != "" {
		cmd.Dir = expand(workingDirectory, stageContext)
		if !filepath.IsAbs(cmd.Dir) {
			cmd.Dir = resolvePath(stageContext.Plan.Project.Root, cmd.Dir)
		}
	}
	cmd.Stdout = stageContext.Stdout
	cmd.Stderr = stageContext.Stderr
	cmd.Stdin = os.Stdin
	contextPath, err := writeContext(stageContext)
	if err != nil {
		return err
	}
	cmd.Env = append(os.Environ(),
		"WAILS_BUILD_STAGE="+stageContext.Stage.Reference(),
		"WAILS_BUILD_CONTEXT="+contextPath,
	)
	for name, value := range environment {
		cmd.Env = append(cmd.Env, name+"="+expand(value, stageContext))
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(expanded, " "), err)
	}
	return nil
}

func writeContext(stageContext *StageContext) (string, error) {
	directory := filepath.Join(stageContext.Plan.Project.Root, ".wails", "build", "context")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", fmt.Errorf("create hook context directory: %w", err)
	}
	path := filepath.Join(directory, SafeInstanceName(stageContext.Stage.Reference())+".json")
	payload := struct {
		Stage      Stage               `json:"stage"`
		Targets    []Target            `json:"targets"`
		Artifacts  map[string]Artifact `json:"artifacts"`
		HookInputs map[string]string   `json:"hookInputs,omitempty"`
	}{
		Stage:      stageContext.Stage,
		Targets:    stageContext.Plan.Targets,
		Artifacts:  stageContext.Artifacts,
		HookInputs: stageContext.HookInputs,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal hook context: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write hook context: %w", err)
	}
	return path, nil
}

func expand(value string, stageContext *StageContext) string {
	replacements := map[string]string{
		"${project.root}": stageContext.Plan.Project.Root,
	}
	if stageContext.Stage.Target != nil {
		replacements["${target.platform}"] = stageContext.Stage.Target.Platform
		replacements["${target.arch}"] = stageContext.Stage.Target.Arch
	} else if len(stageContext.Plan.Targets) == 1 {
		replacements["${target.platform}"] = stageContext.Plan.Targets[0].Platform
		replacements["${target.arch}"] = stageContext.Plan.Targets[0].Arch
	}
	for name, artifact := range stageContext.Artifacts {
		replacements["${artifacts."+name+"}"] = resolvePath(stageContext.Plan.Project.Root, artifact.Path)
	}
	for from, to := range replacements {
		value = strings.ReplaceAll(value, from, to)
	}
	return value
}

// SafeInstanceName converts a qualified stage instance to a portable filename.
func SafeInstanceName(instance string) string {
	replacer := strings.NewReplacer(".", "-", "[", "-", "]", "", "/", "-")
	return replacer.Replace(instance)
}

func resolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, filepath.FromSlash(path))
}

func notifyStage(options ExecuteOptions, stage Stage, status string) {
	if options.OnStage != nil {
		options.callbackMu.Lock()
		defer options.callbackMu.Unlock()
		options.OnStage(stage, status)
	}
}

func notifyAction(options ExecuteOptions, stage Stage, action Action, status string) {
	if options.OnAction != nil {
		options.callbackMu.Lock()
		defer options.callbackMu.Unlock()
		options.OnAction(stage, action, status)
	}
}
