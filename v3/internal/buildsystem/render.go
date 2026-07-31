package buildsystem

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type StageInspection struct {
	Version string  `json:"version"`
	Project Project `json:"project"`
	Target  Target  `json:"target"`
	Mode    string  `json:"mode"`
	Stage   Stage   `json:"stage"`
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
	for _, stage := range plan.Stages {
		if stage.ID == id {
			return &StageInspection{
				Version: plan.Version,
				Project: plan.Project,
				Target:  plan.Target,
				Mode:    plan.Mode,
				Stage:   stage,
			}, nil
		}
	}
	return nil, fmt.Errorf("unknown build stage %q", id)
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
	if _, err := fmt.Fprintf(writer, "Build step:     %s\n", stage.ID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		writer,
		"Target:         %s/%s (%s)\n",
		inspection.Target.Platform,
		inspection.Target.Arch,
		inspection.Mode,
	); err != nil {
		return err
	}
	if len(inspection.Target.Tags) > 0 {
		if _, err := fmt.Fprintf(writer, "Tags:           %s\n", strings.Join(inspection.Target.Tags, ", ")); err != nil {
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

func writeHooks(writer io.Writer, hooks []Hook) error {
	for _, hook := range hooks {
		name := hook.Name
		if name == "" {
			name = strings.Join(hook.Command, " ")
		}
		if _, err := fmt.Fprintf(writer, "  %s\n", name); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(writer, "    command: %s\n", strings.Join(hook.Command, " ")); err != nil {
			return err
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
	}
	return nil
}

func writeArtifacts(writer io.Writer, artifacts []Artifact) error {
	for _, artifact := range artifacts {
		path := artifact.Path
		if path == "" {
			path = "(in-memory)"
		}
		if _, err := fmt.Fprintf(writer, "  %s (%s): %s\n", artifact.Name, artifact.Type, path); err != nil {
			return err
		}
	}
	return nil
}

func WriteText(writer io.Writer, plan *Plan) error {
	if _, err := fmt.Fprintf(writer, "Build plan: %s/%s (%s)\n", plan.Target.Platform, plan.Target.Arch, plan.Mode); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Project:    %s\n", plan.Project.Name); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Binary:     %s\n", plan.Project.BinaryName); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Output:     %s\n", plan.Project.Output); err != nil {
		return err
	}
	if len(plan.Target.Tags) > 0 {
		if _, err := fmt.Fprintf(writer, "Tags:       %s\n", strings.Join(plan.Target.Tags, ", ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(writer, "\nStages:"); err != nil {
		return err
	}
	for _, stage := range plan.Stages {
		line := fmt.Sprintf("  %-22s %-7s", stage.ID, stage.Status)
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
			if _, err := fmt.Fprintf(writer, "    -> %s (%s): %s\n", output.Name, output.Type, output.Path); err != nil {
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
