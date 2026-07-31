// Package buildsystem models the Wails-owned build pipeline.
//
// The package is deliberately independent of Taskfiles. It is the typed
// boundary that plan inspection and, later, stage execution share.
package buildsystem

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// PlanVersion remains at 1 while the unreleased plan contract is evolving.
// Increment it only when compatibility with a released schema is required.
const PlanVersion = "1"

type Request struct {
	ProjectRoot string
	ConfigPath  string
	Targets     []Target
	Target      string
	Arch        string
	Mode        string
	Tags        []string
	Obfuscated  bool
}

type Plan struct {
	Version     string   `json:"version"`
	Project     Project  `json:"project"`
	Targets     []Target `json:"targets"`
	Mode        string   `json:"mode"`
	Goal        string   `json:"goal"`
	Stages      []Stage  `json:"stages"`
	Diagnostics []string `json:"diagnostics,omitempty"`
}

type Project struct {
	Root           string `json:"root"`
	Config         string `json:"config,omitempty"`
	Name           string `json:"name"`
	BinaryName     string `json:"binaryName"`
	Output         string `json:"output"`
	Frontend       string `json:"frontend"`
	PackageManager string `json:"packageManager"`
}

type Target struct {
	Platform string   `yaml:"platform" json:"platform"`
	Arch     string   `yaml:"arch"     json:"arch"`
	Tags     []string `yaml:"tags"     json:"tags"`
}

type Stage struct {
	ID             string     `json:"id"`
	Instance       string     `json:"instance"`
	Target         *Target    `json:"target,omitempty"`
	Implementation string     `json:"implementation"`
	Needs          []string   `json:"needs,omitempty"`
	Status         string     `json:"status"`
	Reason         string     `json:"reason,omitempty"`
	Inputs         []Artifact `json:"inputs,omitempty"`
	Outputs        []Artifact `json:"outputs,omitempty"`
	Actions        []Action   `json:"actions,omitempty"`
	Before         []Hook     `json:"before,omitempty"`
	After          []Hook     `json:"after,omitempty"`
	Replacement    *Command   `json:"replacement,omitempty"`
}

type Artifact struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Path     string          `json:"path,omitempty"`
	Producer string          `json:"producer,omitempty"`
	Target   *ArtifactTarget `json:"target,omitempty"`
}

// ArtifactTarget identifies the target-specific variant of an artifact.
// Format is populated by fan-out stages such as package.create.
type ArtifactTarget struct {
	Platform string `json:"platform,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Format   string `json:"format,omitempty"`
}

type ActionKind string

const (
	ActionCommand   ActionKind = "command"
	ActionCheckTool ActionKind = "check-tool"
	ActionCopy      ActionKind = "copy"
	ActionInternal  ActionKind = "internal"
	ActionMkdir     ActionKind = "mkdir"
	ActionRemove    ActionKind = "remove"
	ActionVerify    ActionKind = "verify"
)

// Action is a resolved, executable operation within a public build stage.
// Actions are inspectable implementation details, not stable extension IDs.
type Action struct {
	Kind             ActionKind        `json:"kind"`
	Description      string            `json:"description,omitempty"`
	Status           string            `json:"status,omitempty"`
	Reason           string            `json:"reason,omitempty"`
	Finally          bool              `json:"finally,omitempty"`
	Command          []string          `json:"command,omitempty"`
	WorkingDirectory string            `json:"workingDirectory,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	Timeout          string            `json:"timeout,omitempty"`
	Source           string            `json:"source,omitempty"`
	Destination      string            `json:"destination,omitempty"`
	Path             string            `json:"path,omitempty"`
	Internal         string            `json:"internal,omitempty"`
	Tool             string            `json:"tool,omitempty"`
	Parameters       map[string]string `json:"parameters,omitempty"`
}

type Hook struct {
	Name             string            `yaml:"name"                       json:"name,omitempty"`
	Command          []string          `yaml:"command"                    json:"command"`
	WorkingDirectory string            `yaml:"workingDirectory,omitempty" json:"workingDirectory,omitempty"`
	Scope            string            `yaml:"scope,omitempty"            json:"scope,omitempty"`
	Environment      map[string]string `yaml:"env,omitempty"              json:"environment,omitempty"`
	Sources          []string          `yaml:"sources,omitempty"          json:"sources,omitempty"`
	Timeout          string            `yaml:"timeout,omitempty"          json:"timeout,omitempty"`
}

type Command struct {
	Command          []string          `yaml:"command"                    json:"command"`
	Args             []string          `yaml:"args,omitempty"             json:"args,omitempty"`
	WorkingDirectory string            `yaml:"workingDirectory,omitempty" json:"workingDirectory,omitempty"`
	Environment      map[string]string `yaml:"env,omitempty"              json:"environment,omitempty"`
	Produces         map[string]string `yaml:"produces"                   json:"produces"`
}

type stageConfig struct {
	Before  []Hook   `yaml:"before"`
	After   []Hook   `yaml:"after"`
	Replace *Command `yaml:"replace"`
	Matrix  struct {
		Exclude []TargetSelector `yaml:"exclude"`
	} `yaml:"matrix"`
}

type TargetSelector struct {
	Platform string `yaml:"platform" json:"platform,omitempty"`
	Arch     string `yaml:"arch"     json:"arch,omitempty"`
}

type projectConfig struct {
	Info struct {
		ProductName string `yaml:"productName"`
	} `yaml:"info"`
	Build struct {
		BinaryName string   `yaml:"binaryName"`
		Output     string   `yaml:"output"`
		Tags       []string `yaml:"tags"`
		Targets    []Target `yaml:"targets"`
		Frontend   struct {
			Directory      string `yaml:"directory"`
			PackageManager string `yaml:"packageManager"`
			Output         string `yaml:"output"`
		} `yaml:"frontend"`
		Stages map[string]stageConfig `yaml:"stages"`
	} `yaml:"build"`
}

func Resolve(request Request) (*Plan, error) {
	root := request.ProjectRoot
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve project root: %w", err)
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}

	configPath := request.ConfigPath
	if configPath == "" {
		configPath = filepath.Join("build", "config.yml")
	}
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(root, configPath)
	}

	cfg, configFound, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}

	name := cfg.Info.ProductName
	if name == "" {
		name = filepath.Base(root)
	}
	binaryName := cfg.Build.BinaryName
	if binaryName == "" {
		binaryName = normaliseName(filepath.Base(root))
	}
	output := cfg.Build.Output
	if output == "" {
		output = "bin"
	}
	frontend := cfg.Build.Frontend.Directory
	if frontend == "" {
		frontend = "frontend"
	}
	packageManager := cfg.Build.Frontend.PackageManager
	if packageManager == "" {
		packageManager = detectPackageManager(filepath.Join(root, frontend))
	}
	frontendOutput := cfg.Build.Frontend.Output
	if frontendOutput == "" {
		frontendOutput = filepath.Join(frontend, "dist")
	}

	mode := request.Mode
	if mode == "" {
		mode = "production"
	}
	tags := mergeTags(cfg.Build.Tags, request.Tags)
	if mode == "production" {
		tags = mergeTags(tags, []string{"production"})
	}
	if request.Obfuscated {
		tags = mergeTags(tags, []string{"wails_obfuscated"})
	}
	if len(request.Targets) == 0 && request.Target == "" && request.Arch == "" && len(cfg.Build.Targets) > 0 {
		request.Targets = cfg.Build.Targets
	}
	targets, err := resolveTargets(request, tags)
	if err != nil {
		return nil, err
	}
	targets = excludeTargets(targets, cfg.Build.Stages["native.compile"].Matrix.Exclude)
	if len(targets) == 0 {
		return nil, fmt.Errorf("build target matrix is empty after exclusions")
	}

	plan := &Plan{
		Version: PlanVersion,
		Project: Project{
			Root:           root,
			Name:           name,
			BinaryName:     binaryName,
			Output:         output,
			Frontend:       frontend,
			PackageManager: packageManager,
		},
		Targets: targets,
		Mode:    mode,
		Goal:    "build",
	}
	plan.Stages = defaultStages(plan, frontendOutput)
	if err := applyStageConfig(plan.Stages, cfg.Build.Stages); err != nil {
		return nil, err
	}
	normaliseArtifacts(plan)
	if err := Validate(plan); err != nil {
		return nil, err
	}
	if configFound {
		plan.Project.Config = configPath
	} else {
		plan.Diagnostics = append(plan.Diagnostics,
			fmt.Sprintf("configuration %s was not found; defaults were used", configPath))
	}
	return plan, nil
}

func applyStageConfig(stages []Stage, configured map[string]stageConfig) error {
	for id, config := range configured {
		var indexes []int
		for index := range stages {
			if stages[index].ID == id {
				indexes = append(indexes, index)
			}
		}
		if len(indexes) == 0 {
			return fmt.Errorf("build configuration references unknown stage %q", id)
		}
		for _, hook := range append(slices.Clone(config.Before), config.After...) {
			if len(hook.Command) == 0 {
				return fmt.Errorf("stage %q contains a hook without a command", id)
			}
		}
		normaliseHooks(config.Before)
		normaliseHooks(config.After)
		for _, index := range indexes {
			stages[index].Before = slices.Clone(config.Before)
			stages[index].After = slices.Clone(config.After)
			if config.Replace == nil {
				continue
			}
			if len(config.Replace.Command) == 0 {
				return fmt.Errorf("replacement for stage %q has no command", id)
			}
			if len(config.Replace.Produces) == 0 {
				return fmt.Errorf("replacement for stage %q declares no produced artifacts", id)
			}
			for _, output := range stages[index].Outputs {
				if _, ok := config.Replace.Produces[output.Name]; !ok {
					return fmt.Errorf("replacement for stage %q does not produce required artifact %q", id, output.Name)
				}
			}
			stages[index].Implementation = "command"
			stages[index].Replacement = config.Replace
			for name, path := range config.Replace.Produces {
				path = expandTargetPath(path, stages[index].Target)
				outputIndex := slices.IndexFunc(stages[index].Outputs, func(output Artifact) bool {
					return output.Name == name
				})
				if outputIndex < 0 {
					stages[index].Outputs = append(stages[index].Outputs, Artifact{
						Name:   name,
						Type:   "custom",
						Path:   filepath.ToSlash(path),
						Target: artifactTarget(stages[index].Target),
					})
					continue
				}
				stages[index].Outputs[outputIndex].Path = filepath.ToSlash(path)
			}
		}
	}
	return nil
}

func expandTargetPath(path string, target *Target) string {
	if target == nil {
		return path
	}
	path = strings.ReplaceAll(path, "${target.platform}", target.Platform)
	return strings.ReplaceAll(path, "${target.arch}", target.Arch)
}

func normaliseHooks(hooks []Hook) {
	for index := range hooks {
		if hooks[index].Scope == "" {
			hooks[index].Scope = "stage"
		}
		if hooks[index].WorkingDirectory == "" {
			hooks[index].WorkingDirectory = "${project.root}"
		}
	}
}

func loadConfig(path string) (projectConfig, bool, error) {
	var cfg projectConfig
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, false, nil
	}
	if err != nil {
		return cfg, false, fmt.Errorf("read build configuration %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, false, fmt.Errorf("parse build configuration %s: %w", path, err)
	}
	return cfg, true, nil
}

func detectPackageManager(frontendDir string) string {
	for _, candidate := range []struct {
		lockfile string
		name     string
	}{
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"bun.lock", "bun"},
		{"bun.lockb", "bun"},
		{"package-lock.json", "npm"},
	} {
		if _, err := os.Stat(filepath.Join(frontendDir, candidate.lockfile)); err == nil {
			return candidate.name
		}
	}
	return "npm"
}

func mergeTags(groups ...[]string) []string {
	var result []string
	for _, group := range groups {
		for _, value := range group {
			for tag := range strings.SplitSeq(value, ",") {
				tag = strings.TrimSpace(tag)
				if tag != "" && !slices.Contains(result, tag) {
					result = append(result, tag)
				}
			}
		}
	}
	return result
}

func supportedTarget(target string) bool {
	return slices.Contains([]string{"windows", "darwin", "linux", "android", "ios"}, target)
}

func normaliseName(name string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "-"))
}
