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
	"time"

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
	Goal        string
	Packages    []string
}

type Plan struct {
	Version     string        `json:"version"`
	Project     Project       `json:"project"`
	Targets     []Target      `json:"targets"`
	Mode        string        `json:"mode"`
	Goal        string        `json:"goal"`
	Signing     SigningConfig `json:"signing,omitempty"`
	Stages      []Stage       `json:"stages"`
	Diagnostics []string      `json:"diagnostics,omitempty"`
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
	ID             string         `json:"id"`
	Instance       string         `json:"instance"`
	Target         *Target        `json:"target,omitempty"`
	Implementation string         `json:"implementation"`
	Uses           string         `json:"uses,omitempty"`
	Needs          []string       `json:"needs,omitempty"`
	Status         string         `json:"status"`
	Reason         string         `json:"reason,omitempty"`
	Inputs         []Artifact     `json:"inputs,omitempty"`
	Outputs        []Artifact     `json:"outputs,omitempty"`
	Actions        []Action       `json:"actions,omitempty"`
	Before         []Hook         `json:"before,omitempty"`
	After          []Hook         `json:"after,omitempty"`
	Replacement    *Command       `json:"replacement,omitempty"`
	Settings       map[string]any `json:"settings,omitempty"`
	Cache          CachePolicy    `json:"cache"`
}

func (stage Stage) Operation() string {
	if stage.Uses != "" {
		return strings.TrimPrefix(stage.Uses, "wails/")
	}
	return stage.ID
}

type CachePolicy struct {
	Enabled     bool              `json:"enabled"`
	Sources     []string          `json:"sources,omitempty"`
	Exclusions  []string          `json:"exclusions,omitempty"`
	Environment []string          `json:"environment,omitempty"`
	Values      map[string]string `json:"values,omitempty"`
}

type Artifact struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Path     string          `json:"path,omitempty"`
	Producer string          `json:"producer,omitempty"`
	Target   *ArtifactTarget `json:"target,omitempty"`
	Optional bool            `json:"optional,omitempty"`
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
	Optional         bool              `json:"optional,omitempty"`
	Recursive        bool              `json:"recursive,omitempty"`
	Command          []string          `json:"command,omitempty"`
	Shell            bool              `json:"shell,omitempty"`
	ResolvedCommand  []string          `json:"resolvedCommand,omitempty"`
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
	ID               string            `yaml:"-"                          json:"id,omitempty"`
	Name             string            `yaml:"name"                       json:"name,omitempty"`
	Command          []string          `yaml:"command"                    json:"command"`
	Shell            bool              `yaml:"shell,omitempty"            json:"shell,omitempty"`
	ResolvedCommand  []string          `yaml:"-"                          json:"resolvedCommand,omitempty"`
	WorkingDirectory string            `yaml:"workingDirectory,omitempty" json:"workingDirectory,omitempty"`
	Scope            string            `yaml:"scope,omitempty"            json:"scope,omitempty"`
	Environment      map[string]string `yaml:"env,omitempty"              json:"environment,omitempty"`
	Inputs           map[string]string `yaml:"inputs,omitempty"           json:"inputs,omitempty"`
	Outputs          map[string]string `yaml:"outputs,omitempty"          json:"outputs,omitempty"`
	Sources          []string          `yaml:"sources,omitempty"          json:"sources,omitempty"`
	Cache            HookCacheInputs   `yaml:"cache,omitempty"            json:"cache,omitempty"`
	When             HookCondition     `yaml:"when,omitempty"             json:"when,omitempty"`
	OnFailure        string            `yaml:"onFailure,omitempty"        json:"onFailure,omitempty"`
	Timeout          string            `yaml:"timeout,omitempty"          json:"timeout,omitempty"`
	Status           string            `yaml:"-"                          json:"status"`
	Reason           string            `yaml:"-"                          json:"reason,omitempty"`
}

type HookCacheInputs struct {
	Files       []string          `yaml:"files,omitempty"       json:"files,omitempty"`
	Environment []string          `yaml:"environment,omitempty" json:"environment,omitempty"`
	Values      map[string]string `yaml:"values,omitempty"      json:"values,omitempty"`
}

type HookCondition struct {
	Platform    StringList        `yaml:"platform,omitempty"    json:"platform,omitempty"`
	Arch        StringList        `yaml:"arch,omitempty"        json:"arch,omitempty"`
	Mode        StringList        `yaml:"mode,omitempty"        json:"mode,omitempty"`
	Format      StringList        `yaml:"format,omitempty"      json:"format,omitempty"`
	Environment map[string]string `yaml:"env,omitempty"         json:"environment,omitempty"`
}

type StringList []string

func (values *StringList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var value string
		if err := node.Decode(&value); err != nil {
			return err
		}
		*values = []string{value}
		return nil
	}
	var result []string
	if err := node.Decode(&result); err != nil {
		return err
	}
	*values = result
	return nil
}

type Command struct {
	Command          []string          `yaml:"command"                    json:"command"`
	Args             []string          `yaml:"args,omitempty"             json:"args,omitempty"`
	WorkingDirectory string            `yaml:"workingDirectory,omitempty" json:"workingDirectory,omitempty"`
	Environment      map[string]string `yaml:"env,omitempty"              json:"environment,omitempty"`
	Produces         map[string]string `yaml:"produces"                   json:"produces"`
}

type stageConfig struct {
	Before   []Hook         `yaml:"before"`
	After    []Hook         `yaml:"after"`
	Replace  *Command       `yaml:"replace"`
	Settings map[string]any `yaml:"settings"`
	Matrix   struct {
		Exclude []TargetSelector `yaml:"exclude"`
	} `yaml:"matrix"`
}

type TargetSelector struct {
	Platform string `yaml:"platform" json:"platform,omitempty"`
	Arch     string `yaml:"arch"     json:"arch,omitempty"`
}

type PackageConfig struct {
	Format string `yaml:"format" json:"format"`
}

type SigningConfig struct {
	Credentials struct {
		Provider string `yaml:"provider" json:"provider,omitempty"`
		Prefix   string `yaml:"prefix" json:"prefix,omitempty"`
	} `yaml:"credentials" json:"credentials,omitempty"`
	Darwin struct {
		Identity        string `yaml:"identity" json:"identity,omitempty"`
		Entitlements    string `yaml:"entitlements" json:"entitlements,omitempty"`
		KeychainProfile string `yaml:"keychainProfile" json:"keychainProfile,omitempty"`
	} `yaml:"darwin" json:"darwin,omitempty"`
	Windows struct {
		Certificate     string `yaml:"certificate" json:"certificate,omitempty"`
		Thumbprint      string `yaml:"thumbprint" json:"thumbprint,omitempty"`
		TimestampServer string `yaml:"timestampServer" json:"timestampServer,omitempty"`
	} `yaml:"windows" json:"windows,omitempty"`
	Linux struct {
		PGPKey string `yaml:"pgpKey" json:"pgpKey,omitempty"`
		Role   string `yaml:"role" json:"role,omitempty"`
	} `yaml:"linux" json:"linux,omitempty"`
}

type projectConfig struct {
	Info struct {
		ProductName string `yaml:"productName"`
	} `yaml:"info"`
	Build struct {
		BinaryName string          `yaml:"binaryName"`
		Output     string          `yaml:"output"`
		Tags       []string        `yaml:"tags"`
		Targets    []Target        `yaml:"targets"`
		Packages   []PackageConfig `yaml:"packages"`
		Signing    SigningConfig   `yaml:"signing"`
		Frontend   struct {
			Directory      string `yaml:"directory"`
			PackageManager string `yaml:"packageManager"`
			Output         string `yaml:"output"`
		} `yaml:"frontend"`
		Stages   map[string]stageConfig `yaml:"stages"`
		Pipeline ExplicitPipeline       `yaml:"pipeline"`
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

	goal := request.Goal
	if goal == "" {
		goal = "build"
	}
	switch goal {
	case "build", "package", "sign", "notarize":
	default:
		return nil, fmt.Errorf("unsupported build goal %q", goal)
	}
	packages := request.Packages
	if len(packages) == 0 {
		for _, configured := range cfg.Build.Packages {
			packages = append(packages, configured.Format)
		}
	}
	packages, err = normalisePackageFormats(packages)
	if err != nil {
		return nil, err
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
		Goal:    goal,
		Signing: cfg.Build.Signing,
	}
	if provider := plan.Signing.Credentials.Provider; provider != "" && provider != "environment" && provider != "keychain" {
		return nil, fmt.Errorf("unsupported signing credential provider %q", provider)
	}
	if len(cfg.Build.Pipeline) > 0 {
		if len(cfg.Build.Stages) > 0 {
			return nil, fmt.Errorf("build.stages cannot be combined with build.pipeline")
		}
		plan.Stages, err = resolveExplicitPipeline(plan, cfg.Build.Pipeline, frontendOutput, packages)
		if err != nil {
			return nil, err
		}
	} else {
		plan.Stages = defaultStages(plan, frontendOutput, packages)
	}
	normaliseCachePolicies(plan)
	if len(cfg.Build.Pipeline) > 0 {
		applyExplicitCacheSources(plan.Stages, cfg.Build.Pipeline)
	}
	if len(cfg.Build.Pipeline) == 0 {
		err = applyStageConfig(plan, cfg.Build.Stages)
	}
	if err != nil {
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

func normaliseCachePolicies(plan *Plan) {
	for index := range plan.Stages {
		stage := &plan.Stages[index]
		stage.Cache = CachePolicy{
			Enabled:    cacheableStage(*stage),
			Sources:    []string{"${project.root}"},
			Exclusions: []string{".git", ".wails", ".beads", "node_modules", plan.Project.Output},
		}
		if stage.Operation() == "package.create" && stage.Target != nil && stage.Target.Platform == "android" {
			stage.Cache.Environment = []string{
				"ANDROID_KEYSTORE_FILE", "ANDROID_KEYSTORE_PASSWORD", "ANDROID_KEY_ALIAS", "ANDROID_KEY_PASSWORD",
			}
		}
	}
}

func cacheableStage(stage Stage) bool {
	if len(stage.Outputs) == 0 {
		return false
	}
	switch stage.Operation() {
	case "bundle.sign", "package.sign", "package.notarize":
		return false
	default:
		return true
	}
}

func normalisePackageFormats(formats []string) ([]string, error) {
	result := make([]string, 0, len(formats))
	seen := make(map[string]bool, len(formats))
	for _, format := range formats {
		format = strings.ToLower(strings.TrimSpace(format))
		if format == "" {
			return nil, fmt.Errorf("package format must not be empty")
		}
		if seen[format] {
			return nil, fmt.Errorf("duplicate package format %q", format)
		}
		seen[format] = true
		result = append(result, format)
	}
	return result, nil
}

func applyStageConfig(plan *Plan, configured map[string]stageConfig) error {
	stages := plan.Stages
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
		if err := validateStageSettings(id, config.Settings); err != nil {
			return err
		}
		for _, hook := range append(slices.Clone(config.Before), config.After...) {
			if len(hook.Command) == 0 {
				return fmt.Errorf("stage %q contains a hook without a command", id)
			}
		}
		if err := normaliseHooks(id, "before", config.Before); err != nil {
			return err
		}
		if err := normaliseHooks(id, "after", config.After); err != nil {
			return err
		}
		for _, index := range indexes {
			stages[index].Settings = cloneSettings(config.Settings)
			if id == "frontend.build" {
				if output, ok := config.Settings["output"].(string); ok {
					for outputIndex := range stages[index].Outputs {
						if stages[index].Outputs[outputIndex].Name == "frontend" {
							stages[index].Outputs[outputIndex].Path = filepath.ToSlash(output)
						}
					}
				}
			}
			stages[index].Before = resolveHooks(plan, stages[index], config.Before)
			stages[index].After = resolveHooks(plan, stages[index], config.After)
			appendHookOutputs(&stages[index], stages[index].Before)
			appendHookOutputs(&stages[index], stages[index].After)
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
	plan.Stages = stages
	synchroniseArtifactPaths(plan)
	return nil
}

func synchroniseArtifactPaths(plan *Plan) {
	outputs := make(map[string]Artifact)
	for _, stage := range plan.Stages {
		for _, output := range stage.Outputs {
			outputs[output.Producer+"\x00"+output.Name] = output
		}
	}
	for stageIndex := range plan.Stages {
		for inputIndex := range plan.Stages[stageIndex].Inputs {
			input := &plan.Stages[stageIndex].Inputs[inputIndex]
			if output, ok := outputs[input.Producer+"\x00"+input.Name]; ok {
				input.Path = output.Path
			}
		}
	}
}

func expandTargetPath(path string, target *Target) string {
	if target == nil {
		return path
	}
	path = strings.ReplaceAll(path, "${target.platform}", target.Platform)
	return strings.ReplaceAll(path, "${target.arch}", target.Arch)
}

func normaliseHooks(stageID, position string, hooks []Hook) error {
	for index := range hooks {
		hooks[index].ID = fmt.Sprintf("%s.%s.%d", stageID, position, index)
		if hooks[index].Scope == "" {
			hooks[index].Scope = "stage"
		}
		switch hooks[index].Scope {
		case "stage", "build", "target", "architecture", "package":
		default:
			return fmt.Errorf("stage %q hook %q has unsupported scope %q", stageID, hooks[index].Name, hooks[index].Scope)
		}
		if hooks[index].OnFailure == "" {
			hooks[index].OnFailure = "fail"
		}
		if hooks[index].OnFailure != "fail" && hooks[index].OnFailure != "continue" {
			return fmt.Errorf("stage %q hook %q has unsupported failure behavior %q", stageID, hooks[index].Name, hooks[index].OnFailure)
		}
		if hooks[index].Timeout != "" {
			if _, err := time.ParseDuration(hooks[index].Timeout); err != nil {
				return fmt.Errorf("stage %q hook %q has invalid timeout %q: %w", stageID, hooks[index].Name, hooks[index].Timeout, err)
			}
		}
		if hooks[index].WorkingDirectory == "" {
			hooks[index].WorkingDirectory = "${project.root}"
		}
		hooks[index].Cache.Files = mergeUnique(hooks[index].Cache.Files, hooks[index].Sources)
		hooks[index].Sources = nil
		hooks[index].Status = "planned"
		if hooks[index].Shell {
			hooks[index].ResolvedCommand = shellCommand(hooks[index].Command)
		}
	}
	return nil
}

func shellCommand(command []string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd.exe", "/S", "/C", strings.Join(command, " ")}
	}
	return []string{"/bin/sh", "-c", strings.Join(command, " ")}
}

func mergeUnique(groups ...[]string) []string {
	var result []string
	for _, group := range groups {
		for _, value := range group {
			if value != "" && !slices.Contains(result, value) {
				result = append(result, value)
			}
		}
	}
	return result
}

func validateStageSettings(stageID string, settings map[string]any) error {
	if len(settings) == 0 {
		return nil
	}
	allowed := map[string]map[string]string{
		"dependencies.prepare": {"goModules": "string", "frontend": "string"},
		"frontend.build":       {"directory": "string", "packageManager": "string", "install": "command", "build": "command", "output": "string", "environment": "map"},
		"platform.generate":    {"overlays": "map"},
		"native.compile":       {"tags": "strings", "trimPath": "bool", "vcsInfo": "bool"},
	}[stageID]
	if allowed == nil {
		return fmt.Errorf("stage %q does not support declarative settings", stageID)
	}
	for name, value := range settings {
		kind, ok := allowed[name]
		if !ok {
			return fmt.Errorf("stage %q has unknown setting %q", stageID, name)
		}
		valid := false
		switch kind {
		case "string":
			_, valid = value.(string)
		case "bool":
			_, valid = value.(bool)
		case "map":
			_, valid = value.(map[string]any)
		case "strings":
			valid = stringSequence(value)
		case "command":
			group, isMap := value.(map[string]any)
			valid = isMap && stringSequence(group["command"])
		}
		if !valid {
			return fmt.Errorf("stage %q setting %q must be %s", stageID, name, kind)
		}
	}
	if value, ok := settings["goModules"].(string); ok && !slices.Contains([]string{"none", "validate", "download", "tidy"}, value) {
		return fmt.Errorf("stage %q setting %q has unsupported value %q", stageID, "goModules", value)
	}
	if value, ok := settings["frontend"].(string); ok && !slices.Contains([]string{"none", "install", "install-if-needed"}, value) {
		return fmt.Errorf("stage %q setting %q has unsupported value %q", stageID, "frontend", value)
	}
	return nil
}

func stringSequence(value any) bool {
	switch values := value.(type) {
	case []string:
		return true
	case []any:
		for _, value := range values {
			if _, ok := value.(string); !ok {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func resolveHooks(plan *Plan, stage Stage, hooks []Hook) []Hook {
	result := slices.Clone(hooks)
	for index := range result {
		if reason := hookConditionMismatch(plan, stage, result[index].When); reason != "" {
			result[index].Status = "skipped"
			result[index].Reason = reason
		}
	}
	return result
}

func hookConditionMismatch(plan *Plan, stage Stage, condition HookCondition) string {
	platform, arch, format := "", "", ""
	if stage.Target != nil {
		platform, arch = stage.Target.Platform, stage.Target.Arch
	}
	for _, artifact := range append(slices.Clone(stage.Inputs), stage.Outputs...) {
		if artifact.Target != nil && artifact.Target.Format != "" {
			format = artifact.Target.Format
			break
		}
	}
	checks := []struct {
		name   string
		actual string
		values StringList
	}{
		{"platform", platform, condition.Platform},
		{"architecture", arch, condition.Arch},
		{"mode", plan.Mode, condition.Mode},
		{"package format", format, condition.Format},
	}
	for _, check := range checks {
		if len(check.values) > 0 && !slices.Contains(check.values, check.actual) {
			return fmt.Sprintf("%s %q does not match %s", check.name, check.actual, strings.Join(check.values, ", "))
		}
	}
	for name, expected := range condition.Environment {
		if actual := os.Getenv(name); actual != expected {
			return fmt.Sprintf("environment %s does not match configured value", name)
		}
	}
	return ""
}

func appendHookOutputs(stage *Stage, hooks []Hook) {
	for _, hook := range hooks {
		if hook.Status == "skipped" {
			continue
		}
		for name, path := range hook.Outputs {
			stage.Outputs = append(stage.Outputs, Artifact{
				Name: name, Type: "custom", Path: filepath.ToSlash(expandTargetPath(path, stage.Target)),
				Target: artifactTarget(stage.Target), Optional: hook.OnFailure == "continue",
			})
		}
	}
}

func cloneSettings(settings map[string]any) map[string]any {
	if settings == nil {
		return nil
	}
	result := make(map[string]any, len(settings))
	for name, value := range settings {
		result[name] = cloneSettingValue(value)
	}
	return result
}

func cloneSettingValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneSettings(value)
	case []any:
		result := make([]any, len(value))
		for index := range value {
			result[index] = cloneSettingValue(value[index])
		}
		return result
	default:
		return value
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

func UsesExplicitPipeline(path string) (bool, error) {
	if path == "" {
		path = filepath.Join("build", "config.yml")
	}
	config, found, err := loadConfig(path)
	if err != nil || !found {
		return false, err
	}
	return len(config.Build.Pipeline) > 0, nil
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
