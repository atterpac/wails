package buildsystem

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

func resolveTargets(request Request, tags []string) ([]Target, error) {
	targets := request.Targets
	if len(targets) == 0 {
		platform := request.Target
		if platform == "" {
			platform = runtime.GOOS
		}
		arch := request.Arch
		if arch == "" {
			arch = runtime.GOARCH
		}
		targets = []Target{{Platform: platform, Arch: arch}}
	}

	result := make([]Target, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		if !supportedTarget(target.Platform) {
			return nil, fmt.Errorf("unsupported build target %q", target.Platform)
		}
		if target.Arch == "" {
			return nil, fmt.Errorf("build architecture must not be empty")
		}
		key := target.Platform + "/" + target.Arch
		if seen[key] {
			return nil, fmt.Errorf("duplicate build target %q", key)
		}
		seen[key] = true
		target.Tags = mergeTags(tags, target.Tags)
		result = append(result, target)
	}
	return result, nil
}

func excludeTargets(targets []Target, exclusions []TargetSelector) []Target {
	result := make([]Target, 0, len(targets))
	for _, target := range targets {
		excluded := false
		for _, exclusion := range exclusions {
			platformMatches := exclusion.Platform == "" || exclusion.Platform == target.Platform
			archMatches := exclusion.Arch == "" || exclusion.Arch == target.Arch
			if platformMatches && archMatches {
				excluded = true
				break
			}
		}
		if !excluded {
			result = append(result, target)
		}
	}
	return result
}

func defaultStages(plan *Plan, frontendOutput string) []Stage {
	sharedArtifact := func(name, kind, path, producer string) Artifact {
		return Artifact{
			ID:       name,
			Name:     name,
			Type:     kind,
			Path:     filepath.ToSlash(path),
			Producer: producer,
		}
	}
	targetArtifact := func(name, kind, path, producer string, target Target) Artifact {
		artifact := sharedArtifact(name, kind, path, producer)
		artifact.Target = artifactTarget(&target)
		artifact.ID = artifactIdentity(artifact)
		return artifact
	}
	planned := func(id string, target *Target, needs []string, inputs, outputs []Artifact) Stage {
		return Stage{
			ID:             id,
			Instance:       stageInstance(id, target),
			Target:         cloneTarget(target),
			Implementation: "wails/" + id,
			Needs:          needs,
			Status:         "planned",
			Inputs:         inputs,
			Outputs:        outputs,
		}
	}
	skipped := func(id string, target *Target, needs []string, reason string) Stage {
		stage := planned(id, target, needs, nil, nil)
		stage.Status = "skipped"
		stage.Reason = reason
		return stage
	}

	project := planned("project.resolve", nil, nil, nil, []Artifact{
		sharedArtifact("plan", "build-plan", "", "project.resolve"),
	})
	stages := []Stage{project}

	toolchains := make([]string, 0, len(plan.Targets))
	for _, target := range plan.Targets {
		instance := stageInstance("toolchain.check", &target)
		toolchains = append(toolchains, instance)
		stages = append(stages, planned("toolchain.check", &target, []string{project.Reference()}, nil, []Artifact{
			targetArtifact("capabilities", "capability-report", "", instance, target),
		}))
	}

	dependencies := planned("dependencies.prepare", nil, toolchains, nil, []Artifact{
		sharedArtifact("dependencies", "dependency-state", "", "dependencies.prepare"),
	})
	bindings := planned("bindings.generate", nil, []string{dependencies.Reference()}, nil, []Artifact{
		sharedArtifact(
			"bindings",
			"frontend-bindings",
			filepath.Join(plan.Project.Frontend, "bindings"),
			"bindings.generate",
		),
	})
	frontend := planned("frontend.build", nil, []string{dependencies.Reference(), bindings.Reference()}, []Artifact{
		bindings.Outputs[0],
	}, []Artifact{
		sharedArtifact("frontend", "frontend-distribution", frontendOutput, "frontend.build"),
	})
	stages = append(stages, dependencies, bindings)

	assets := make(map[string]Stage, len(plan.Targets))
	for _, target := range plan.Targets {
		key := targetKey(target)
		output := filepath.Join(".wails", "build", target.Platform, target.Arch, "assets")
		producer := stageInstance("assets.generate", &target)
		stage := planned(
			"assets.generate",
			&target,
			[]string{stageInstance("toolchain.check", &target)},
			nil,
			[]Artifact{targetArtifact("assets", "platform-assets", output, producer, target)},
		)
		assets[key] = stage
		stages = append(stages, stage)
	}
	stages = append(stages, frontend)

	platforms := make(map[string]Stage, len(plan.Targets))
	for _, target := range plan.Targets {
		key := targetKey(target)
		output := filepath.Join(".wails", "build", target.Platform, target.Arch, "platform")
		producer := stageInstance("platform.generate", &target)
		stage := planned(
			"platform.generate",
			&target,
			[]string{assets[key].Reference()},
			[]Artifact{assets[key].Outputs[0]},
			[]Artifact{targetArtifact("platform", "platform-resources", output, producer, target)},
		)
		platforms[key] = stage
		stages = append(stages, stage)
	}

	binaries := make([]Artifact, 0, len(plan.Targets))
	nativeStages := make(map[string]Stage, len(plan.Targets))
	for _, target := range plan.Targets {
		key := targetKey(target)
		producer := stageInstance("native.compile", &target)
		binary := targetArtifact(
			"binary",
			"native-binary",
			binaryPath(plan, target),
			producer,
			target,
		)
		stage := planned(
			"native.compile",
			&target,
			[]string{frontend.Reference(), platforms[key].Reference()},
			[]Artifact{frontend.Outputs[0], bindings.Outputs[0], platforms[key].Outputs[0]},
			[]Artifact{binary},
		)
		nativeStages[key] = stage
		binaries = append(binaries, binary)
		stages = append(stages, stage)
	}

	platformCounts := make(map[string]int)
	for _, target := range plan.Targets {
		platformCounts[target.Platform]++
	}
	for _, target := range plan.Targets {
		key := targetKey(target)
		combineReason := "single-architecture build"
		if target.Platform == "darwin" && platformCounts[target.Platform] > 1 {
			combineReason = "multi-architecture combination is not yet implemented"
		}
		combine := skipped(
			"binary.combine",
			&target,
			[]string{nativeStages[key].Reference()},
			combineReason,
		)
		bundle := skipped(
			"bundle.assemble",
			&target,
			[]string{nativeStages[key].Reference(), combine.Reference()},
			"not selected by the build goal",
		)
		bundleSign := skipped(
			"bundle.sign",
			&target,
			[]string{bundle.Reference()},
			"not selected by the build goal",
		)
		packageCreate := skipped(
			"package.create",
			&target,
			[]string{bundleSign.Reference()},
			"not selected by the build goal",
		)
		packageSign := skipped(
			"package.sign",
			&target,
			[]string{packageCreate.Reference()},
			"not selected by the build goal",
		)
		packageNotarize := skipped(
			"package.notarize",
			&target,
			[]string{packageSign.Reference()},
			"not selected by the build goal",
		)
		stages = append(stages, combine, bundle, bundleSign, packageCreate, packageSign, packageNotarize)
	}

	nativeNeeds := make([]string, 0, len(nativeStages))
	for _, target := range plan.Targets {
		nativeNeeds = append(nativeNeeds, nativeStages[targetKey(target)].Reference())
	}
	stages = append(stages, planned(
		"artifacts.collect",
		nil,
		nativeNeeds,
		binaries,
		[]Artifact{sharedArtifact(
			"manifest",
			"artifact-manifest",
			filepath.Join(plan.Project.Output, "artifacts.json"),
			"artifacts.collect",
		)},
	))
	return stages
}

func binaryPath(plan *Plan, target Target) string {
	filename := plan.Project.BinaryName
	if target.Platform == "windows" && !strings.EqualFold(filepath.Ext(filename), ".exe") {
		filename += ".exe"
	}
	if len(plan.Targets) == 1 {
		return filepath.Join(plan.Project.Output, filename)
	}
	return filepath.Join(plan.Project.Output, target.Platform, target.Arch, filename)
}

func stageInstance(id string, target *Target) string {
	if target == nil {
		return id
	}
	return id + "[" + targetKey(*target) + "]"
}

func targetKey(target Target) string {
	return target.Platform + "/" + target.Arch
}

func cloneTarget(target *Target) *Target {
	if target == nil {
		return nil
	}
	result := *target
	result.Tags = append([]string(nil), target.Tags...)
	return &result
}

func artifactTarget(target *Target) *ArtifactTarget {
	if target == nil {
		return nil
	}
	return &ArtifactTarget{Platform: target.Platform, Arch: target.Arch}
}
