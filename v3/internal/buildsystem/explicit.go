package buildsystem

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type ExplicitPipeline []ExplicitPipelineEntry

type ExplicitPipelineEntry struct {
	Name   string
	Config ExplicitStageConfig
}

type ExplicitStageConfig struct {
	Uses             string            `yaml:"uses"`
	Run              []string          `yaml:"run"`
	Needs            []string          `yaml:"needs"`
	With             map[string]any    `yaml:"with"`
	Produces         map[string]string `yaml:"produces"`
	Sources          []string          `yaml:"sources"`
	WorkingDirectory string            `yaml:"workingDirectory"`
	Environment      map[string]string `yaml:"env"`
	Timeout          string            `yaml:"timeout"`
	Shell            bool              `yaml:"shell"`
}

func (pipeline *ExplicitPipeline) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("build.pipeline must be a mapping")
	}
	result := make(ExplicitPipeline, 0, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		name := node.Content[index].Value
		if name == "" {
			return fmt.Errorf("build.pipeline contains an empty stage name")
		}
		if slices.ContainsFunc(result, func(entry ExplicitPipelineEntry) bool { return entry.Name == name }) {
			return fmt.Errorf("build.pipeline contains duplicate stage %q", name)
		}
		var config ExplicitStageConfig
		if err := node.Content[index+1].Decode(&config); err != nil {
			return fmt.Errorf("decode pipeline stage %q: %w", name, err)
		}
		result = append(result, ExplicitPipelineEntry{Name: name, Config: config})
	}
	*pipeline = result
	return nil
}

func resolveExplicitPipeline(
	plan *Plan,
	pipeline ExplicitPipeline,
	frontendOutput string,
	packageFormats []string,
) ([]Stage, error) {
	templatePlan := *plan
	templatePlan.Goal = "notarize"
	templates := defaultStages(&templatePlan, frontendOutput, packageFormats)
	instances := make(map[string][]Stage, len(pipeline))
	configs := make(map[string]ExplicitStageConfig, len(pipeline))
	var stages []Stage

	for _, entry := range pipeline {
		config := entry.Config
		configs[entry.Name] = config
		if (config.Uses == "") == (len(config.Run) == 0) {
			return nil, fmt.Errorf("pipeline stage %q must declare exactly one of uses or run", entry.Name)
		}
		if config.Uses != "" && !strings.HasPrefix(config.Uses, "wails/") {
			return nil, fmt.Errorf("pipeline stage %q has unsupported implementation %q", entry.Name, config.Uses)
		}
		if config.Timeout != "" {
			if _, err := time.ParseDuration(config.Timeout); err != nil {
				return nil, fmt.Errorf("pipeline stage %q has invalid timeout %q: %w", entry.Name, config.Timeout, err)
			}
		}

		var expanded []Stage
		if config.Uses == "" {
			expanded = []Stage{{
				ID: entry.Name, Instance: entry.Name, Implementation: "command", Status: "planned",
				Actions: []Action{{
					Kind: ActionCommand, Description: "Run user-owned pipeline stage " + entry.Name,
					Status: "planned", Command: slices.Clone(config.Run), Shell: config.Shell,
					WorkingDirectory: config.WorkingDirectory, Environment: cloneStringMap(config.Environment), Timeout: config.Timeout,
				}},
			}}
			if config.Shell {
				expanded[0].Actions[0].ResolvedCommand = shellCommand(config.Run)
			}
			if expanded[0].Actions[0].WorkingDirectory == "" {
				expanded[0].Actions[0].WorkingDirectory = plan.Project.Root
			}
		} else {
			operation := strings.TrimPrefix(config.Uses, "wails/")
			if err := validateStageSettings(operation, config.With); err != nil {
				return nil, fmt.Errorf("pipeline stage %q: %w", entry.Name, err)
			}
			for _, template := range templates {
				if template.ID != operation || template.Status != "planned" {
					continue
				}
				stage := template
				stage.ID = entry.Name
				stage.Instance = explicitInstance(entry.Name, template.Reference(), operation)
				stage.Uses = config.Uses
				stage.Implementation = config.Uses
				stage.Needs = nil
				stage.Inputs = nil
				stage.Before = nil
				stage.After = nil
				stage.Replacement = nil
				stage.Actions = nil
				stage.Settings = cloneSettings(config.With)
				for outputIndex := range stage.Outputs {
					stage.Outputs[outputIndex].Producer = stage.Reference()
				}
				expanded = append(expanded, stage)
			}
			if len(expanded) == 0 {
				return nil, fmt.Errorf("pipeline stage %q uses unavailable implementation %q for the selected targets", entry.Name, config.Uses)
			}
		}

		for index := range expanded {
			applyExplicitOutputs(plan, &expanded[index], config.Produces)
		}
		instances[entry.Name] = expanded
		stages = append(stages, expanded...)
	}

	for index := range stages {
		config := configs[stages[index].ID]
		for _, logicalDependency := range config.Needs {
			candidates, ok := instances[logicalDependency]
			if !ok {
				return nil, fmt.Errorf("pipeline stage %q requires unknown stage %q", stages[index].ID, logicalDependency)
			}
			matched := matchingExplicitDependencies(stages[index], candidates)
			if len(matched) == 0 {
				return nil, fmt.Errorf("pipeline stage %q has no target-compatible instance of dependency %q", stages[index].Reference(), logicalDependency)
			}
			for _, dependency := range matched {
				stages[index].Needs = append(stages[index].Needs, dependency.Reference())
				stages[index].Inputs = append(stages[index].Inputs, cloneArtifactsSlice(dependency.Outputs)...)
			}
		}
	}
	return stages, nil
}

func applyExplicitCacheSources(stages []Stage, pipeline ExplicitPipeline) {
	sources := make(map[string][]string, len(pipeline))
	for _, entry := range pipeline {
		sources[entry.Name] = entry.Config.Sources
	}
	for index := range stages {
		if configured := sources[stages[index].ID]; len(configured) > 0 {
			stages[index].Cache.Sources = slices.Clone(configured)
		}
	}
}

func explicitInstance(name, templateReference, operation string) string {
	suffix := strings.TrimPrefix(templateReference, operation)
	return name + suffix
}

func matchingExplicitDependencies(stage Stage, candidates []Stage) []Stage {
	if stage.Target == nil {
		return slices.Clone(candidates)
	}
	var result []Stage
	for _, candidate := range candidates {
		if candidate.Target == nil || candidate.Target.Platform == stage.Target.Platform && candidate.Target.Arch == stage.Target.Arch {
			result = append(result, candidate)
		}
	}
	return result
}

func applyExplicitOutputs(plan *Plan, stage *Stage, configured map[string]string) {
	for name, configuredPath := range configured {
		path := expandExplicitPath(configuredPath, stage)
		index := slices.IndexFunc(stage.Outputs, func(output Artifact) bool { return output.Name == name })
		if index >= 0 {
			stage.Outputs[index].Path = filepath.ToSlash(path)
			continue
		}
		artifact := Artifact{
			Name: name, Type: "custom", Path: filepath.ToSlash(path), Producer: stage.Reference(), Target: artifactTarget(stage.Target),
		}
		if stage.Target == nil && len(plan.Targets) == 1 {
			artifact.Target = nil
		}
		stage.Outputs = append(stage.Outputs, artifact)
	}
}

func expandExplicitPath(path string, stage *Stage) string {
	path = expandTargetPath(path, stage.Target)
	format := ""
	for _, artifact := range stage.Outputs {
		if artifact.Target != nil && artifact.Target.Format != "" {
			format = artifact.Target.Format
			break
		}
	}
	return strings.ReplaceAll(path, "${package.format}", format)
}

func cloneArtifactsSlice(artifacts []Artifact) []Artifact {
	result := make([]Artifact, len(artifacts))
	copy(result, artifacts)
	return result
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for name, value := range values {
		result[name] = value
	}
	return result
}
