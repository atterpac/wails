package buildsystem

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

type StageInspection struct {
	Version string   `json:"version"`
	Project Project  `json:"project"`
	Targets []Target `json:"targets"`
	Mode    string   `json:"mode"`
	Stage   Stage    `json:"stage"`
}

func WriteJSON(writer io.Writer, plan *Plan) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(plan); err != nil {
		return fmt.Errorf("write JSON build plan: %w", err)
	}
	return nil
}

func InspectStage(plan *Plan, id string) (*StageInspection, error) {
	if plan == nil {
		return nil, fmt.Errorf("build plan is nil")
	}
	index, err := resolveStageIndex(plan.Stages, id)
	if err != nil {
		return nil, err
	}
	return &StageInspection{
		Version: plan.Version,
		Project: plan.Project,
		Targets: plan.Targets,
		Mode:    plan.Mode,
		Stage:   plan.Stages[index],
	}, nil
}

func WriteStageJSON(writer io.Writer, inspection *StageInspection) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(inspection); err != nil {
		return fmt.Errorf("write JSON build step inspection: %w", err)
	}
	return nil
}

func WriteStageText(writer io.Writer, inspection *StageInspection) error {
	stage := inspection.Stage
	if _, err := fmt.Fprintf(writer, "Build step:     %s\n", stage.Reference()); err != nil {
		return err
	}
	if stage.Reference() != stage.ID {
		if _, err := fmt.Fprintf(writer, "Public stage:   %s\n", stage.ID); err != nil {
			return err
		}
	}
	if stage.Target != nil {
		if _, err := fmt.Fprintf(
			writer,
			"Target:         %s/%s (%s)\n",
			stage.Target.Platform,
			stage.Target.Arch,
			inspection.Mode,
		); err != nil {
			return err
		}
		if len(stage.Target.Tags) > 0 {
			if _, err := fmt.Fprintf(writer, "Tags:           %s\n", strings.Join(stage.Target.Tags, ", ")); err != nil {
				return err
			}
		}
	} else {
		if _, err := fmt.Fprintf(writer, "Target scope:   shared across %d target(s)\n", len(inspection.Targets)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(writer, "Implementation: %s\n", stage.Implementation); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Status:         %s\n", stage.Status); err != nil {
		return err
	}
	if stage.Reason != "" {
		if _, err := fmt.Fprintf(writer, "Reason:         %s\n", stage.Reason); err != nil {
			return err
		}
	}
	if len(stage.Needs) > 0 {
		if _, err := fmt.Fprintf(writer, "Needs:          %s\n", strings.Join(stage.Needs, ", ")); err != nil {
			return err
		}
	}
	if len(stage.Settings) > 0 {
		settings, err := json.Marshal(stage.Settings)
		if err != nil {
			return fmt.Errorf("encode stage settings: %w", err)
		}
		if _, err := fmt.Fprintf(writer, "Settings:      %s\n", settings); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(writer, "Cache:          %t\n", stage.Cache.Enabled); err != nil {
		return err
	}
	if stage.Cache.Enabled && len(stage.Cache.Sources) > 0 {
		if _, err := fmt.Fprintf(writer, "Cache sources:  %s\n", strings.Join(stage.Cache.Sources, ", ")); err != nil {
			return err
		}
	}
	if stage.Cache.Enabled && len(stage.Cache.Exclusions) > 0 {
		if _, err := fmt.Fprintf(writer, "Cache excludes: %s\n", strings.Join(stage.Cache.Exclusions, ", ")); err != nil {
			return err
		}
	}

	if len(stage.Before) > 0 {
		if _, err := fmt.Fprintln(writer, "\nBefore hooks:"); err != nil {
			return err
		}
		if err := writeHooks(writer, stage.Before); err != nil {
			return err
		}
	}
	if stage.Replacement != nil {
		if _, err := fmt.Fprintln(writer, "\nReplacement:"); err != nil {
			return err
		}
		command := make([]string, 0, len(stage.Replacement.Command)+len(stage.Replacement.Args))
		command = append(command, stage.Replacement.Command...)
		command = append(command, stage.Replacement.Args...)
		if _, err := fmt.Fprintf(writer, "  command: %s\n", strings.Join(command, " ")); err != nil {
			return err
		}
		if stage.Replacement.WorkingDirectory != "" {
			if _, err := fmt.Fprintf(writer, "  directory: %s\n", stage.Replacement.WorkingDirectory); err != nil {
				return err
			}
		}
	}
	if len(stage.Actions) > 0 {
		if _, err := fmt.Fprintln(writer, "\nExecution:"); err != nil {
			return err
		}
		if err := writeActions(writer, stage.Actions); err != nil {
			return err
		}
	}
	if len(stage.After) > 0 {
		if _, err := fmt.Fprintln(writer, "\nAfter hooks:"); err != nil {
			return err
		}
		if err := writeHooks(writer, stage.After); err != nil {
			return err
		}
	}
	if len(stage.Inputs) > 0 {
		if _, err := fmt.Fprintln(writer, "\nInputs:"); err != nil {
			return err
		}
		if err := writeArtifacts(writer, stage.Inputs); err != nil {
			return err
		}
	}
	if len(stage.Outputs) > 0 {
		if _, err := fmt.Fprintln(writer, "\nOutputs:"); err != nil {
			return err
		}
		if err := writeArtifacts(writer, stage.Outputs); err != nil {
			return err
		}
	}
	return nil
}

func writeActions(writer io.Writer, actions []Action) error {
	for index, action := range actions {
		status := action.Status
		if status == "" {
			status = "planned"
		}
		if _, err := fmt.Fprintf(writer, "  %d. %s [%s]", index+1, action.Kind, status); err != nil {
			return err
		}
		if action.Description != "" {
			if _, err := fmt.Fprintf(writer, " %s", action.Description); err != nil {
				return err
			}
		}
		if action.Finally {
			if _, err := fmt.Fprint(writer, " [finally]"); err != nil {
				return err
			}
		}
		if action.Optional {
			if _, err := fmt.Fprint(writer, " [optional]"); err != nil {
				return err
			}
		}
		if action.Recursive {
			if _, err := fmt.Fprint(writer, " [recursive]"); err != nil {
				return err
			}
		}
		if action.Reason != "" {
			if _, err := fmt.Fprintf(writer, " (%s)", action.Reason); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(writer); err != nil {
			return err
		}

		switch action.Kind {
		case ActionCommand:
			if _, err := fmt.Fprintf(writer, "     command: %s\n", formatCommand(action.Command)); err != nil {
				return err
			}
		case ActionCheckTool:
			if _, err := fmt.Fprintf(writer, "     tool: %s\n", action.Tool); err != nil {
				return err
			}
		case ActionCopy:
			if _, err := fmt.Fprintf(writer, "     from: %s\n     to:   %s\n", action.Source, action.Destination); err != nil {
				return err
			}
		case ActionInternal:
			if _, err := fmt.Fprintf(writer, "     operation: %s\n", action.Internal); err != nil {
				return err
			}
		case ActionMkdir, ActionRemove, ActionVerify:
			if _, err := fmt.Fprintf(writer, "     path: %s\n", action.Path); err != nil {
				return err
			}
		}
		if len(action.Parameters) > 0 {
			if _, err := fmt.Fprintln(writer, "     parameters:"); err != nil {
				return err
			}
			names := make([]string, 0, len(action.Parameters))
			for name := range action.Parameters {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if _, err := fmt.Fprintf(writer, "       %s=%s\n", name, action.Parameters[name]); err != nil {
					return err
				}
			}
		}
		if action.WorkingDirectory != "" {
			if _, err := fmt.Fprintf(writer, "     directory: %s\n", action.WorkingDirectory); err != nil {
				return err
			}
		}
		if len(action.Environment) > 0 {
			if _, err := fmt.Fprintln(writer, "     environment:"); err != nil {
				return err
			}
			names := make([]string, 0, len(action.Environment))
			for name := range action.Environment {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if _, err := fmt.Fprintf(writer, "       %s=%s\n", name, action.Environment[name]); err != nil {
					return err
				}
			}
		}
		if action.Timeout != "" {
			if _, err := fmt.Fprintf(writer, "     timeout: %s\n", action.Timeout); err != nil {
				return err
			}
		}
	}
	return nil
}

func formatCommand(command []string) string {
	quoted := make([]string, len(command))
	for index, argument := range command {
		quoted[index] = strconv.Quote(argument)
	}
	return strings.Join(quoted, " ")
}

func writeHooks(writer io.Writer, hooks []Hook) error {
	for _, hook := range hooks {
		name := hook.Name
		if name == "" {
			name = strings.Join(hook.Command, " ")
		}
		status := hook.Status
		if status == "" {
			status = "planned"
		}
		if _, err := fmt.Fprintf(writer, "  %s [%s]\n", name, status); err != nil {
			return err
		}
		if hook.Reason != "" {
			if _, err := fmt.Fprintf(writer, "    reason: %s\n", hook.Reason); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(writer, "    command: %s\n", formatCommand(hook.Command)); err != nil {
			return err
		}
		if hook.Shell {
			if _, err := fmt.Fprintln(writer, "    shell: true"); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(writer, "    execution: %s\n", formatCommand(hook.ResolvedCommand)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(writer, "    scope: %s\n", hook.Scope); err != nil {
			return err
		}
		if hook.WorkingDirectory != "" {
			if _, err := fmt.Fprintf(writer, "    directory: %s\n", hook.WorkingDirectory); err != nil {
				return err
			}
		}
		if hook.Timeout != "" {
			if _, err := fmt.Fprintf(writer, "    timeout: %s\n", hook.Timeout); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(writer, "    on failure: %s\n", hook.OnFailure); err != nil {
			return err
		}
		for _, field := range []struct {
			name   string
			values map[string]string
		}{{"inputs", hook.Inputs}, {"outputs", hook.Outputs}, {"environment", hook.Environment}, {"cache values", hook.Cache.Values}} {
			if len(field.values) == 0 {
				continue
			}
			if _, err := fmt.Fprintf(writer, "    %s:\n", field.name); err != nil {
				return err
			}
			if err := writeStringMap(writer, "      ", field.values); err != nil {
				return err
			}
		}
		if len(hook.Cache.Files) > 0 {
			if _, err := fmt.Fprintf(writer, "    cache files: %s\n", strings.Join(hook.Cache.Files, ", ")); err != nil {
				return err
			}
		}
		if len(hook.Cache.Environment) > 0 {
			if _, err := fmt.Fprintf(writer, "    cache environment: %s\n", strings.Join(hook.Cache.Environment, ", ")); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeStringMap(writer io.Writer, indentation string, values map[string]string) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := fmt.Fprintf(writer, "%s%s=%s\n", indentation, name, values[name]); err != nil {
			return err
		}
	}
	return nil
}

func writeArtifacts(writer io.Writer, artifacts []Artifact) error {
	for _, artifact := range artifacts {
		path := artifact.Path
		if path == "" {
			path = "(in-memory)"
		}
		if _, err := fmt.Fprintf(
			writer,
			"  %s (%s): %s\n",
			artifact.Reference(),
			artifact.Type,
			path,
		); err != nil {
			return err
		}
		if artifact.Producer != "" {
			if _, err := fmt.Fprintf(writer, "    producer: %s\n", artifact.Producer); err != nil {
				return err
			}
		}
	}
	return nil
}

func WriteText(writer io.Writer, plan *Plan) error {
	if _, err := fmt.Fprintf(writer, "Build plan: %d target(s) (%s)\n", len(plan.Targets), plan.Mode); err != nil {
		return err
	}
	for _, target := range plan.Targets {
		if _, err := fmt.Fprintf(writer, "  - %s/%s", target.Platform, target.Arch); err != nil {
			return err
		}
		if len(target.Tags) > 0 {
			if _, err := fmt.Fprintf(writer, " [%s]", strings.Join(target.Tags, ", ")); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(writer); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(writer, "Project:    %s\n", plan.Project.Name); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Goal:       %s\n", plan.Goal); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Binary:     %s\n", plan.Project.BinaryName); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Output:     %s\n", plan.Project.Output); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, "\nStages:"); err != nil {
		return err
	}
	for _, stage := range plan.Stages {
		line := fmt.Sprintf("  %-38s %-7s", stage.Reference(), stage.Status)
		if stage.Cache.Enabled {
			line += "  cacheable"
		}
		if stage.Reason != "" {
			line += "  " + stage.Reason
		} else if stage.Replacement != nil {
			line += "  replaced by: " + strings.Join(stage.Replacement.Command, " ")
		}
		if _, err := fmt.Fprintln(writer, line); err != nil {
			return err
		}
		for _, hook := range stage.Before {
			if _, err := fmt.Fprintf(writer, "    before: %s\n", strings.Join(hook.Command, " ")); err != nil {
				return err
			}
		}
		for _, hook := range stage.After {
			if _, err := fmt.Fprintf(writer, "    after:  %s\n", strings.Join(hook.Command, " ")); err != nil {
				return err
			}
		}
		for _, output := range stage.Outputs {
			if output.Path == "" {
				continue
			}
			if _, err := fmt.Fprintf(
				writer,
				"    -> %s (%s): %s\n",
				output.Reference(),
				output.Type,
				output.Path,
			); err != nil {
				return err
			}
		}
	}
	for _, diagnostic := range plan.Diagnostics {
		if _, err := fmt.Fprintf(writer, "\nNote: %s\n", diagnostic); err != nil {
			return err
		}
	}
	return nil
}
