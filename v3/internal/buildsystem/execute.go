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
	"time"
)

type StageFunc func(context.Context, *StageContext) error
type ActionFunc func(context.Context, *StageContext, Action) error

type ExecuteOptions struct {
	From            string
	Until           string
	Step            string
	Stdout          io.Writer
	Stderr          io.Writer
	Builtins        map[string]StageFunc
	InternalActions map[string]ActionFunc
	OnStage         func(Stage, string)
	OnAction        func(Stage, Action, string)
}

type StageContext struct {
	Plan      *Plan
	Stage     Stage
	Artifacts map[string]Artifact
	Stdout    io.Writer
	Stderr    io.Writer
}

func Execute(ctx context.Context, plan *Plan, options ExecuteOptions) error {
	if plan == nil {
		return errors.New("build plan is nil")
	}
	if options.Stdout == nil {
		options.Stdout = os.Stdout
	}
	if options.Stderr == nil {
		options.Stderr = os.Stderr
	}

	first, last, err := executionRange(plan.Stages, options)
	if err != nil {
		return err
	}
	artifacts := collectArtifacts(plan.Stages[:first])

	for index := first; index <= last; index++ {
		stage := plan.Stages[index]
		if stage.Status == "skipped" {
			notifyStage(options, stage, "skipped")
			continue
		}

		stageContext := &StageContext{
			Plan:      plan,
			Stage:     stage,
			Artifacts: artifacts,
			Stdout:    options.Stdout,
			Stderr:    options.Stderr,
		}
		if err := verifyInputs(plan.Project.Root, stage); err != nil {
			return err
		}
		notifyStage(options, stage, "running")
		if err := executeHooks(ctx, stageContext, stage.Before); err != nil {
			return fmt.Errorf("stage %s before hook: %w", stage.ID, err)
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
				return fmt.Errorf("stage %s replacement: %w", stage.ID, err)
			}
		} else if len(stage.Actions) > 0 {
			if err := executeActions(ctx, stageContext, options); err != nil {
				return fmt.Errorf("stage %s: %w", stage.ID, err)
			}
		} else {
			implementation := options.Builtins[stage.ID]
			if implementation == nil {
				return fmt.Errorf("stage %s has no built-in implementation", stage.ID)
			}
			if err := implementation(ctx, stageContext); err != nil {
				return fmt.Errorf("stage %s: %w", stage.ID, err)
			}
		}
		for _, output := range stage.Outputs {
			stageContext.Artifacts[output.Name] = output
		}
		if err := executeHooks(ctx, stageContext, stage.After); err != nil {
			return fmt.Errorf("stage %s after hook: %w", stage.ID, err)
		}
		if err := verifyOutputs(plan.Project.Root, stage); err != nil {
			return err
		}
		notifyStage(options, stage, "completed")
	}
	return nil
}

func executeActions(ctx context.Context, stageContext *StageContext, options ExecuteOptions) error {
	actions := stageContext.Stage.Actions
	for index, action := range actions {
		if action.Status == "skipped" {
			notifyAction(options, stageContext.Stage, action, "skipped")
			continue
		}
		notifyAction(options, stageContext.Stage, action, "running")
		if err := executeAction(ctx, stageContext, action, options.InternalActions); err != nil {
			cleanupErr := executeFinalActions(ctx, stageContext, options, actions[index+1:])
			return errors.Join(err, cleanupErr)
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
	var result error
	for _, action := range actions {
		if !action.Finally || action.Status == "skipped" {
			continue
		}
		notifyAction(options, stageContext.Stage, action, "running")
		if err := executeAction(ctx, stageContext, action, options.InternalActions); err != nil {
			result = errors.Join(result, err)
			continue
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
		return executeCommand(
			ctx,
			stageContext,
			action.Command,
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
		return copyFile(
			expand(action.Source, stageContext),
			expand(action.Destination, stageContext),
		)
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

func executionRange(stages []Stage, options ExecuteOptions) (int, int, error) {
	first, last := 0, len(stages)-1
	if options.Step != "" {
		index := stageIndex(stages, options.Step)
		if index < 0 {
			return 0, 0, fmt.Errorf("unknown build stage %q", options.Step)
		}
		return index, index, nil
	}
	if options.From != "" {
		first = stageIndex(stages, options.From)
		if first < 0 {
			return 0, 0, fmt.Errorf("unknown --from stage %q", options.From)
		}
	}
	if options.Until != "" {
		last = stageIndex(stages, options.Until)
		if last < 0 {
			return 0, 0, fmt.Errorf("unknown --until stage %q", options.Until)
		}
	}
	if first > last {
		return 0, 0, fmt.Errorf("--from stage %q comes after --until stage %q", options.From, options.Until)
	}
	return first, last, nil
}

func stageIndex(stages []Stage, id string) int {
	for index, stage := range stages {
		if stage.ID == id {
			return index
		}
	}
	return -1
}

func collectArtifacts(stages []Stage) map[string]Artifact {
	result := make(map[string]Artifact)
	for _, stage := range stages {
		for _, output := range stage.Outputs {
			result[output.Name] = output
		}
	}
	return result
}

func verifyInputs(root string, stage Stage) error {
	for _, input := range stage.Inputs {
		if input.Path == "" {
			continue
		}
		if _, err := os.Stat(resolvePath(root, input.Path)); err != nil {
			return fmt.Errorf("stage %s requires artifact %q at %s: %w", stage.ID, input.Name, input.Path, err)
		}
	}
	return nil
}

func verifyOutputs(root string, stage Stage) error {
	for _, output := range stage.Outputs {
		if output.Path == "" {
			continue
		}
		if _, err := os.Stat(resolvePath(root, output.Path)); err != nil {
			return fmt.Errorf("stage %s did not produce artifact %q at %s: %w", stage.ID, output.Name, output.Path, err)
		}
	}
	return nil
}

func executeHooks(ctx context.Context, stageContext *StageContext, hooks []Hook) error {
	for _, hook := range hooks {
		if err := executeCommand(
			ctx,
			stageContext,
			hook.Command,
			hook.WorkingDirectory,
			hook.Environment,
			hook.Timeout,
		); err != nil {
			name := hook.Name
			if name == "" {
				name = strings.Join(hook.Command, " ")
			}
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
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
		"WAILS_BUILD_STAGE="+stageContext.Stage.ID,
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
	path := filepath.Join(directory, strings.ReplaceAll(stageContext.Stage.ID, ".", "-")+".json")
	payload := struct {
		Stage     Stage               `json:"stage"`
		Target    Target              `json:"target"`
		Artifacts map[string]Artifact `json:"artifacts"`
	}{
		Stage:     stageContext.Stage,
		Target:    stageContext.Plan.Target,
		Artifacts: stageContext.Artifacts,
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
		"${project.root}":    stageContext.Plan.Project.Root,
		"${target.platform}": stageContext.Plan.Target.Platform,
		"${target.arch}":     stageContext.Plan.Target.Arch,
	}
	for name, artifact := range stageContext.Artifacts {
		replacements["${artifacts."+name+"}"] = resolvePath(stageContext.Plan.Project.Root, artifact.Path)
	}
	for from, to := range replacements {
		value = strings.ReplaceAll(value, from, to)
	}
	return value
}

func resolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, filepath.FromSlash(path))
}

func notifyStage(options ExecuteOptions, stage Stage, status string) {
	if options.OnStage != nil {
		options.OnStage(stage, status)
	}
}

func notifyAction(options ExecuteOptions, stage Stage, action Action, status string) {
	if options.OnAction != nil {
		options.OnAction(stage, action, status)
	}
}
