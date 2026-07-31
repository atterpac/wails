package buildsystem

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

type DevPlan struct {
	Version   string       `json:"version"`
	Mode      string       `json:"mode"`
	Project   Project      `json:"project"`
	Target    Target       `json:"target"`
	Watch     DevWatch     `json:"watch"`
	Processes []DevProcess `json:"processes"`
	Build     *Plan        `json:"build"`
}

type DevWatch struct {
	Root               string   `json:"root"`
	DebounceMillis     int      `json:"debounceMillis"`
	IgnoredDirectories []string `json:"ignoredDirectories,omitempty"`
	IgnoredFiles       []string `json:"ignoredFiles,omitempty"`
	WatchedExtensions  []string `json:"watchedExtensions,omitempty"`
	GitIgnore          bool     `json:"gitIgnore"`
}

type DevProcess struct {
	ID               string            `json:"id"`
	Type             string            `json:"type"`
	Command          []string          `json:"command"`
	WorkingDirectory string            `json:"workingDirectory"`
	Environment      map[string]string `json:"environment,omitempty"`
	Readiness        *DevReadiness     `json:"readiness,omitempty"`
	ShutdownTimeout  string            `json:"shutdownTimeout,omitempty"`
	ExitPolicy       string            `json:"exitPolicy,omitempty"`
}

type DevReadiness struct {
	TCP      string `json:"tcp"`
	Timeout  string `json:"timeout"`
	Interval string `json:"interval"`
}

type DevRequest struct {
	CLIPath    string
	ConfigPath string
	Host       string
	Port       int
	Secure     bool
	Tags       []string
}

func ResolveDev(build *Plan, request DevRequest) (*DevPlan, error) {
	if build == nil {
		return nil, fmt.Errorf("development build plan is required")
	}
	if build.Mode != "development" {
		return nil, fmt.Errorf("development plan requires a development-mode build, got %q", build.Mode)
	}
	if len(build.Targets) != 1 {
		return nil, fmt.Errorf("development mode requires exactly one target")
	}
	target := build.Targets[0]
	if target.Platform != "windows" && target.Platform != "darwin" && target.Platform != "linux" {
		return nil, fmt.Errorf("typed development mode does not yet support target %q", target.Platform)
	}
	if request.CLIPath == "" {
		return nil, fmt.Errorf("development mode requires the Wails CLI executable path")
	}
	if request.Host == "" {
		request.Host = "localhost"
	}
	if request.Port <= 0 {
		return nil, fmt.Errorf("development server port must be positive")
	}

	configPath := request.ConfigPath
	if configPath == "" {
		configPath = build.Project.Config
	}
	if configPath == "" {
		configPath = filepath.Join(build.Project.Root, "build", "config.yml")
	}
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(build.Project.Root, configPath)
	}

	compile := []string{request.CLIPath, "build", "--pipeline", "--config", configPath, "DEV=true"}
	if tags := joinNonEmpty(request.Tags); tags != "" {
		compile = append(compile, "--tags", tags)
	}
	frontend, err := devFrontendCommand(build.Project.PackageManager, request.Port)
	if err != nil {
		return nil, err
	}
	runnable, err := devRunnable(build, target)
	if err != nil {
		return nil, err
	}
	scheme := "http"
	if request.Secure {
		scheme = "https"
	}
	environment := map[string]string{
		"WAILS_VITE_PORT":        strconv.Itoa(request.Port),
		"FRONTEND_DEVSERVER_URL": scheme + "://" + request.Host + ":" + strconv.Itoa(request.Port),
	}

	return &DevPlan{
		Version: PlanVersion,
		Mode:    "development",
		Project: build.Project,
		Target:  target,
		Processes: []DevProcess{
			{ID: "dev.compile", Type: "blocking", Command: compile, WorkingDirectory: build.Project.Root, Environment: environment},
			{
				ID: "dev.frontend", Type: "background", Command: frontend,
				WorkingDirectory: filepath.Join(build.Project.Root, build.Project.Frontend), Environment: environment,
				Readiness:       &DevReadiness{TCP: request.Host + ":" + strconv.Itoa(request.Port), Timeout: "30s", Interval: "100ms"},
				ShutdownTimeout: "5s",
				ExitPolicy:      "fail",
			},
			{
				ID: "dev.launch", Type: "primary", Command: []string{runnable},
				WorkingDirectory: build.Project.Root, Environment: environment, ShutdownTimeout: "5s", ExitPolicy: "shutdown",
			},
		},
		Build: build,
	}, nil
}

func devFrontendCommand(packageManager string, port int) ([]string, error) {
	value := strconv.Itoa(port)
	switch packageManager {
	case "npm":
		return []string{"npm", "run", "dev", "--", "--port", value, "--strictPort"}, nil
	case "yarn":
		return []string{"yarn", "dev", "--port", value, "--strictPort"}, nil
	case "pnpm":
		return []string{"pnpm", "dev", "--port", value, "--strictPort"}, nil
	case "bun":
		return []string{"bun", "run", "dev", "--port", value, "--strictPort"}, nil
	default:
		return nil, fmt.Errorf("unsupported frontend package manager %q", packageManager)
	}
}

func devRunnable(plan *Plan, target Target) (string, error) {
	for _, stage := range plan.Stages {
		if stage.Operation() != "bundle.assemble" || stage.Target == nil ||
			stage.Target.Platform != target.Platform || stage.Target.Arch != target.Arch {
			continue
		}
		for _, output := range stage.Outputs {
			if output.Name != "bundle" || output.Path == "" {
				continue
			}
			bundle := output.Path
			if !filepath.IsAbs(bundle) {
				bundle = filepath.Join(plan.Project.Root, bundle)
			}
			switch target.Platform {
			case "windows":
				name := plan.Project.BinaryName
				if !strings.EqualFold(filepath.Ext(name), ".exe") {
					name += ".exe"
				}
				return filepath.Join(bundle, name), nil
			case "darwin":
				return filepath.Join(bundle, "Contents", "MacOS", plan.Project.BinaryName), nil
			case "linux":
				return filepath.Join(bundle, "usr", "bin", plan.Project.BinaryName), nil
			}
		}
	}
	return "", fmt.Errorf("development build does not produce a runnable bundle for %s/%s", target.Platform, target.Arch)
}

func joinNonEmpty(values []string) string {
	result := ""
	for _, value := range values {
		if value == "" {
			continue
		}
		if result != "" {
			result += ","
		}
		result += value
	}
	return result
}
