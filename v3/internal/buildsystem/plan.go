// Package buildsystem models the Wails-owned build pipeline.
//
// The package is deliberately independent of Taskfiles. It is the typed
// boundary that plan inspection and, later, stage execution share.
package buildsystem

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const PlanVersion = "1"

type Request struct {
	ProjectRoot string
	ConfigPath  string
	Target      string
	Arch        string
	Mode        string
	Tags        []string
	Obfuscated  bool
}

type Plan struct {
	Version     string   `json:"version"`
	Project     Project  `json:"project"`
	Target      Target   `json:"target"`
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
	Platform string   `json:"platform"`
	Arch     string   `json:"arch"`
	Tags     []string `json:"tags"`
}

type Stage struct {
	ID             string     `json:"id"`
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
	Name string `json:"name"`
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
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
}

type projectConfig struct {
	Info struct {
		ProductName string `yaml:"productName"`
	} `yaml:"info"`
	Build struct {
		BinaryName string   `yaml:"binaryName"`
		Output     string   `yaml:"output"`
		Tags       []string `yaml:"tags"`
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

	target := request.Target
	if target == "" {
		target = runtime.GOOS
	}
	arch := request.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	if !supportedTarget(target) {
		return nil, fmt.Errorf("unsupported build target %q", target)
	}
	if arch == "" {
		return nil, fmt.Errorf("build architecture must not be empty")
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
	binaryFilename := binaryName
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

	if target == "windows" && !strings.EqualFold(filepath.Ext(binaryFilename), ".exe") {
		binaryFilename += ".exe"
	}
	paths := artifactPaths{
		bindings:       filepath.Join(frontend, "bindings"),
		frontend:       frontendOutput,
		assets:         filepath.Join(".wails", "build", target, arch, "assets"),
		platform:       filepath.Join(".wails", "build", target, arch, "platform"),
		binary:         filepath.Join(output, binaryFilename),
		bundle:         bundlePath(output, name, target),
		artifactReport: filepath.Join(output, "artifacts.json"),
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
		Target: Target{Platform: target, Arch: arch, Tags: tags},
		Mode:   mode,
		Goal:   "build",
		Stages: defaultStages(paths),
	}
	if err := applyStageConfig(plan.Stages, cfg.Build.Stages); err != nil {
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
		index := slices.IndexFunc(stages, func(stage Stage) bool {
			return stage.ID == id
		})
		if index < 0 {
			return fmt.Errorf("build configuration references unknown stage %q", id)
		}
		for _, hook := range append(slices.Clone(config.Before), config.After...) {
			if len(hook.Command) == 0 {
				return fmt.Errorf("stage %q contains a hook without a command", id)
			}
		}
		normaliseHooks(config.Before)
		normaliseHooks(config.After)
		stages[index].Before = config.Before
		stages[index].After = config.After
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
			outputIndex := slices.IndexFunc(stages[index].Outputs, func(output Artifact) bool {
				return output.Name == name
			})
			if outputIndex < 0 {
				stages[index].Outputs = append(stages[index].Outputs, Artifact{
					Name: name,
					Type: "custom",
					Path: filepath.ToSlash(path),
				})
				continue
			}
			stages[index].Outputs[outputIndex].Path = filepath.ToSlash(path)
		}
	}
	return nil
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

type artifactPaths struct {
	bindings       string
	frontend       string
	assets         string
	platform       string
	binary         string
	bundle         string
	artifactReport string
}

func defaultStages(paths artifactPaths) []Stage {
	planned := func(id string, needs []string, inputs, outputs []Artifact) Stage {
		return Stage{
			ID:             id,
			Implementation: "wails/" + id,
			Needs:          needs,
			Status:         "planned",
			Inputs:         inputs,
			Outputs:        outputs,
		}
	}
	skipped := func(id string, needs []string, reason string) Stage {
		return Stage{
			ID:             id,
			Implementation: "wails/" + id,
			Needs:          needs,
			Status:         "skipped",
			Reason:         reason,
		}
	}
	artifact := func(name, kind, path string) Artifact {
		return Artifact{Name: name, Type: kind, Path: filepath.ToSlash(path)}
	}

	return []Stage{
		planned("project.resolve", nil, nil, []Artifact{artifact("plan", "build-plan", "")}),
		planned("toolchain.check", []string{"project.resolve"},
			nil, []Artifact{artifact("capabilities", "capability-report", "")}),
		planned("dependencies.prepare", []string{"toolchain.check"},
			nil, []Artifact{artifact("dependencies", "dependency-state", "")}),
		planned("bindings.generate", []string{"dependencies.prepare"},
			nil, []Artifact{artifact("bindings", "frontend-bindings", paths.bindings)}),
		planned("assets.generate", []string{"toolchain.check"},
			nil, []Artifact{artifact("assets", "platform-assets", paths.assets)}),
		planned("frontend.build", []string{"dependencies.prepare", "bindings.generate"},
			[]Artifact{artifact("bindings", "frontend-bindings", paths.bindings)},
			[]Artifact{artifact("frontend", "frontend-distribution", paths.frontend)}),
		planned("platform.generate", []string{"assets.generate"},
			[]Artifact{artifact("assets", "platform-assets", paths.assets)},
			[]Artifact{artifact("platform", "platform-resources", paths.platform)}),
		planned("native.compile", []string{"frontend.build", "platform.generate"},
			[]Artifact{
				artifact("frontend", "frontend-distribution", paths.frontend),
				artifact("bindings", "frontend-bindings", paths.bindings),
				artifact("platform", "platform-resources", paths.platform),
			},
			[]Artifact{artifact("binary", "native-binary", paths.binary)}),
		skipped("binary.combine", []string{"native.compile"},
			"single-architecture build"),
		skipped("bundle.assemble", []string{"native.compile", "binary.combine"},
			"not selected by the build goal"),
		skipped("bundle.sign", []string{"bundle.assemble"},
			"not selected by the build goal"),
		skipped("package.create", []string{"bundle.sign"},
			"not selected by the build goal"),
		skipped("package.sign", []string{"package.create"},
			"not selected by the build goal"),
		skipped("package.notarize", []string{"package.sign"},
			"not selected by the build goal"),
		planned("artifacts.collect", []string{"native.compile"},
			[]Artifact{artifact("binary", "native-binary", paths.binary)},
			[]Artifact{artifact("manifest", "artifact-manifest", paths.artifactReport)}),
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

func bundlePath(output, name, target string) string {
	switch target {
	case "darwin", "ios":
		return filepath.Join(output, name+".app")
	case "linux":
		return filepath.Join(output, name+".AppDir")
	default:
		return output
	}
}
