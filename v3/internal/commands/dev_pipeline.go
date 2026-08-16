package commands

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/atterpac/refresh/engine"
	"github.com/atterpac/refresh/process"
	"github.com/wailsapp/wails/v3/internal/buildsystem"
	"github.com/wailsapp/wails/v3/internal/flags"
	"gopkg.in/yaml.v3"
)

func resolveTypedDevPlan(options *DevOptions, host string, port int) (*buildsystem.DevPlan, error) {
	watch, err := loadRefreshConfig(options.Config)
	if err != nil {
		return nil, err
	}
	platform, arch, err := devTarget(options.Target, options.Device)
	if err != nil {
		return nil, err
	}
	buildFlags := &flags.Build{Config: options.Config, Pipeline: true, Tags: strings.Join(envTags(), ",")}
	goal := "build"
	var packages []string
	if platform == "android" {
		goal = "package"
		packages = []string{"apk"}
	}
	build, err := buildsystem.Resolve(buildsystem.Request{
		ConfigPath: options.Config,
		Target:     platform,
		Arch:       arch,
		Mode:       "development",
		Tags:       envTags(),
		Goal:       goal,
		Packages:   packages,
	})
	if err != nil {
		return nil, err
	}
	if platform == "ios" && arch == "arm64" && build.Signing.IOS.Identity == "" {
		return nil, fmt.Errorf("iOS device development requires build.signing.ios.identity in %s", build.Project.Config)
	}
	if err := resolveTypedPipelineActions(build, buildFlags, nil); err != nil {
		return nil, err
	}
	cliPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve Wails executable: %w", err)
	}
	plan, err := buildsystem.ResolveDev(build, buildsystem.DevRequest{
		CLIPath: cliPath, ConfigPath: options.Config, Host: host, Port: port,
		Secure: options.Secure, Tags: envTags(), Device: options.Device,
	})
	if err != nil {
		return nil, err
	}
	ensureIgnored(&watch.Ignore.File, "*_test.go")
	plan.Watch = buildsystem.DevWatch{
		Root: watch.RootPath, DebounceMillis: watch.Debounce,
		IgnoredDirectories: append([]string(nil), watch.Ignore.Dir...),
		IgnoredFiles:       append([]string(nil), watch.Ignore.File...),
		WatchedExtensions:  append([]string(nil), watch.Ignore.WatchedExten...),
		GitIgnore:          watch.Ignore.IgnoreGit,
	}
	return plan, nil
}

func devTarget(value, device string) (string, string, error) {
	if value == "" {
		return runtime.GOOS, runtime.GOARCH, nil
	}
	parts := strings.Split(value, "/")
	if len(parts) > 2 || parts[0] == "" {
		return "", "", fmt.Errorf("invalid development target %q; expected platform or platform/architecture", value)
	}
	if len(parts) == 2 && parts[1] != "" {
		return parts[0], parts[1], nil
	}
	switch parts[0] {
	case "android", "ios":
		if device != "" {
			return parts[0], "arm64", nil
		}
		return parts[0], "amd64", nil
	default:
		return parts[0], runtime.GOARCH, nil
	}
}

func executeTypedDevPlan(options *DevOptions, plan *buildsystem.DevPlan) error {
	configuration, err := loadRefreshConfig(options.Config)
	if err != nil {
		return err
	}
	configuration = refreshConfigForDev(configuration, plan)
	supervisor, err := engine.NewEngineFromConfig(configuration)
	if err != nil {
		return err
	}
	return supervisor.Start()
}

func refreshConfigForDev(configuration engine.Config, plan *buildsystem.DevPlan) engine.Config {
	configuration.ExecList = nil
	configuration.BackgroundStruct = process.Execute{}
	configuration.ExecStruct = make([]process.Execute, 0, len(plan.Processes))
	for _, planned := range plan.Processes {
		spec := process.Execute{
			Name: planned.ID, Command: planned.Command, Env: planned.Environment,
			ChangeDir: planned.WorkingDirectory, Type: process.ExecuteType(planned.Type),
			ShutdownTimeout: planned.ShutdownTimeout,
			ExitPolicy:      process.ExitPolicy(planned.ExitPolicy),
		}
		if planned.Readiness != nil {
			spec.Readiness = &process.Readiness{
				TCP: planned.Readiness.TCP, Timeout: planned.Readiness.Timeout, Interval: planned.Readiness.Interval,
			}
		}
		configuration.ExecStruct = append(configuration.ExecStruct, spec)
	}
	ensureIgnored(&configuration.Ignore.File, "*_test.go")
	return configuration
}

func loadRefreshConfig(path string) (engine.Config, error) {
	type developmentConfig struct {
		Config engine.Config `yaml:"dev_mode"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return engine.Config{}, err
	}
	var configured developmentConfig
	if err := yaml.Unmarshal(data, &configured); err != nil {
		return engine.Config{}, err
	}
	return configured.Config, nil
}

func writeDevPlan(writer io.Writer, plan *buildsystem.DevPlan) error {
	if _, err := fmt.Fprintf(writer, "Development plan: %s/%s\n", plan.Target.Platform, plan.Target.Arch); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Watch: %s (%dms debounce; %s)\n", plan.Watch.Root, plan.Watch.DebounceMillis,
		strings.Join(plan.Watch.WatchedExtensions, ", ")); err != nil {
		return err
	}
	for _, planned := range plan.Processes {
		if _, err := fmt.Fprintf(writer, "\n%s (%s)\n  Command: %s\n  Directory: %s\n",
			planned.ID, planned.Type, strings.Join(planned.Command, " "), planned.WorkingDirectory); err != nil {
			return err
		}
		if planned.Readiness != nil {
			if _, err := fmt.Fprintf(writer, "  Readiness: tcp %s (timeout %s, interval %s)\n",
				planned.Readiness.TCP, planned.Readiness.Timeout, planned.Readiness.Interval); err != nil {
				return err
			}
		}
		if planned.ShutdownTimeout != "" {
			if _, err := fmt.Fprintf(writer, "  Shutdown: graceful for %s, then force\n", planned.ShutdownTimeout); err != nil {
				return err
			}
		}
		if planned.ExitPolicy != "" {
			if _, err := fmt.Fprintf(writer, "  Exit: %s\n", planned.ExitPolicy); err != nil {
				return err
			}
		}
	}
	return nil
}
