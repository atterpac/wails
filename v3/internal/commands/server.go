package commands

import (
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/wailsapp/wails/v3/internal/buildsystem"
	"github.com/wailsapp/wails/v3/internal/flags"
)

type ServerBuildOptions struct {
	Config      string `name:"config" description:"Path to the build configuration" default:"build/config.yml"`
	Target      string `name:"target" description:"Server target as platform/architecture"`
	Tags        string `name:"tags" description:"Additional Go build tags"`
	Development bool   `name:"dev" description:"Build a development server binary"`
	Obfuscated  bool   `name:"obfuscated" description:"Build the server with garble"`
	GarbleArgs  string `name:"garbleargs" description:"Additional garble arguments"`
	Docker      bool   `name:"docker" description:"Use the Wails cross-build Docker image"`
	DockerImage string `name:"docker-image" description:"Cross-build Docker image" default:"wails-cross"`
}

type ServerDockerOptions struct {
	Tag          string `name:"tag" description:"Container image tag" default:"wails-server:latest"`
	Port         int    `name:"port" description:"Host port exposed by docker:run" default:"8080"`
	CGOEnabled   bool   `name:"cgo" description:"Enable CGO in the server container build"`
	GoImage      string `name:"go-image" description:"Go builder image"`
	RuntimeImage string `name:"runtime-image" description:"Runtime image"`
}

type CrossDockerOptions struct {
	Image string `name:"image" description:"Cross-build Docker image tag" default:"wails-cross"`
}

func ServerBuild(options *ServerBuildOptions) error {
	targets := []string(nil)
	if options.Target != "" {
		targets = []string{options.Target}
	}
	arguments := []string(nil)
	if options.Development {
		arguments = append(arguments, "DEV=true")
	}
	return Build(&flags.Build{
		Config: options.Config, Targets: targets, Tags: options.Tags, Server: true,
		Obfuscated: options.Obfuscated, GarbleArgs: options.GarbleArgs, Docker: options.Docker, DockerImage: options.DockerImage,
	}, arguments)
}

func ServerRun(options *ServerBuildOptions) error {
	options.Development = true
	if err := ServerBuild(options); err != nil {
		return err
	}
	platform, arch, err := devTarget(options.Target, "")
	if err != nil {
		return err
	}
	if platform != runtime.GOOS || arch != runtime.GOARCH {
		return fmt.Errorf("server run cannot execute cross-built target %s/%s on host %s/%s", platform, arch, runtime.GOOS, runtime.GOARCH)
	}
	plan, err := buildsystem.Resolve(buildsystem.Request{ConfigPath: options.Config, Target: platform, Arch: arch, Mode: "development", Server: true})
	if err != nil {
		return err
	}
	binary := filepath.Join(plan.Project.Root, plan.Project.Output, plan.Project.BinaryName)
	if platform == "windows" {
		binary += ".exe"
	}
	return runMobileCommand(binary)
}

func CrossDockerSetup(options *CrossDockerOptions) error {
	image := options.Image
	if image == "" {
		image = "wails-cross"
	}
	return runMobileCommand("docker", "build", "-t", image, "-f", "build/docker/Dockerfile.cross", "build/docker")
}

func ServerDockerBuild(options *ServerDockerOptions) error {
	arguments := []string{"build", "-t", options.Tag}
	if options.CGOEnabled {
		arguments = append(arguments, "--build-arg", "CGO_ENABLED=1")
	}
	if options.GoImage != "" {
		arguments = append(arguments, "--build-arg", "GO_IMAGE="+options.GoImage)
	}
	if options.RuntimeImage != "" {
		arguments = append(arguments, "--build-arg", "RUNTIME_IMAGE="+options.RuntimeImage)
	}
	arguments = append(arguments, "-f", "build/docker/Dockerfile.server", ".")
	return runMobileCommand("docker", arguments...)
}

func ServerDockerRun(options *ServerDockerOptions) error {
	if err := ServerDockerBuild(options); err != nil {
		return err
	}
	if options.Port <= 0 || options.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return runMobileCommand("docker", "run", "--rm", "-p", fmt.Sprintf("%d:8080", options.Port), options.Tag)
}
