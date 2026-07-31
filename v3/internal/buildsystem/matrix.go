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

func defaultStages(plan *Plan, frontendOutput string, packageFormats []string) []Stage {
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
		stages = append(stages, stage)
	}

	finalArtifacts := make([]Artifact, 0, len(plan.Targets)+1)
	finalNeeds := make([]string, 0, len(plan.Targets)+1)
	darwinTargets := targetsForPlatform(plan.Targets, "darwin")
	var universalCombine *Stage
	if len(darwinTargets) > 1 {
		target := Target{Platform: "darwin", Arch: "universal"}
		for _, sourceTarget := range darwinTargets {
			target.Tags = mergeTags(target.Tags, sourceTarget.Tags)
		}
		producer := stageInstance("binary.combine", &target)
		inputs := make([]Artifact, 0, len(darwinTargets))
		needs := make([]string, 0, len(darwinTargets))
		for _, sourceTarget := range darwinTargets {
			native := nativeStages[targetKey(sourceTarget)]
			inputs = append(inputs, native.Outputs[0])
			needs = append(needs, native.Reference())
		}
		output := targetArtifact(
			"binary",
			"universal-binary",
			binaryPath(plan, target),
			producer,
			target,
		)
		combine := planned("binary.combine", &target, needs, inputs, []Artifact{output})
		universalCombine = &combine
		stages = append(stages, combine)
	}

	for _, target := range plan.Targets {
		key := targetKey(target)
		native := nativeStages[key]
		if target.Platform != "darwin" {
			combine := skipped(
				"binary.combine",
				&target,
				[]string{native.Reference()},
				"target does not require architecture combination",
			)
			bundle := applicationBundleStage(
				plan,
				target,
				native.Outputs[0],
				platforms[key].Outputs[0],
				assets[key].Outputs[0],
				[]string{native.Reference(), combine.Reference(), platforms[key].Reference(), assets[key].Reference()},
				planned,
			)
			stages = append(stages, combine, bundle)
			distribution, artifacts, needs := distributionStages(
				plan, target, bundle.Outputs[0], bundle.Reference(), packageFormats, planned, skipped,
			)
			stages = append(stages, distribution...)
			finalArtifacts = append(finalArtifacts, artifacts...)
			finalNeeds = append(finalNeeds, needs...)
			continue
		}

		if universalCombine != nil {
			continue
		}
		combine := skipped(
			"binary.combine",
			&target,
			[]string{native.Reference()},
			"single-architecture build",
		)
		bundle := darwinBundleStage(
			plan,
			target,
			native.Outputs[0],
			platforms[key].Outputs[0],
			[]string{native.Reference(), combine.Reference(), platforms[key].Reference()},
			planned,
		)
		stages = append(stages, combine, bundle)
		distribution, artifacts, needs := distributionStages(
			plan, target, bundle.Outputs[0], bundle.Reference(), packageFormats, planned, skipped,
		)
		stages = append(stages, distribution...)
		if plan.Goal == "build" {
			finalArtifacts = append(finalArtifacts, native.Outputs[0])
			finalNeeds = append(finalNeeds, native.Reference())
		}
		finalArtifacts = append(finalArtifacts, artifacts...)
		finalNeeds = append(finalNeeds, needs...)
	}

	if universalCombine != nil {
		target := *universalCombine.Target
		platform := platforms[targetKey(darwinTargets[0])]
		bundle := darwinBundleStage(
			plan,
			target,
			universalCombine.Outputs[0],
			platform.Outputs[0],
			[]string{universalCombine.Reference(), platform.Reference()},
			planned,
		)
		stages = append(stages, bundle)
		distribution, artifacts, needs := distributionStages(
			plan, target, bundle.Outputs[0], bundle.Reference(), packageFormats, planned, skipped,
		)
		stages = append(stages, distribution...)
		if plan.Goal == "build" {
			finalArtifacts = append(finalArtifacts, universalCombine.Outputs[0])
			finalNeeds = append(finalNeeds, universalCombine.Reference())
		}
		finalArtifacts = append(finalArtifacts, artifacts...)
		finalNeeds = append(finalNeeds, needs...)
	}

	stages = append(stages, planned(
		"artifacts.collect",
		nil,
		finalNeeds,
		finalArtifacts,
		[]Artifact{sharedArtifact(
			"manifest",
			"artifact-manifest",
			filepath.Join(plan.Project.Output, "artifacts.json"),
			"artifacts.collect",
		)},
	))
	return stages
}

func targetsForPlatform(targets []Target, platform string) []Target {
	result := make([]Target, 0, len(targets))
	for _, target := range targets {
		if target.Platform == platform {
			result = append(result, target)
		}
	}
	return result
}

func darwinBundleStage(
	plan *Plan,
	target Target,
	binary Artifact,
	platform Artifact,
	needs []string,
	planned func(string, *Target, []string, []Artifact, []Artifact) Stage,
) Stage {
	producer := stageInstance("bundle.assemble", &target)
	bundle := Artifact{
		Name:     "bundle",
		Type:     "application-bundle",
		Path:     filepath.ToSlash(bundlePath(plan, target)),
		Producer: producer,
		Target:   artifactTarget(&target),
	}
	bundle.ID = artifactIdentity(bundle)
	return planned("bundle.assemble", &target, needs, []Artifact{binary, platform}, []Artifact{bundle})
}

func applicationBundleStage(
	plan *Plan,
	target Target,
	binary Artifact,
	platform Artifact,
	assets Artifact,
	needs []string,
	planned func(string, *Target, []string, []Artifact, []Artifact) Stage,
) Stage {
	producer := stageInstance("bundle.assemble", &target)
	bundle := Artifact{
		Name: "bundle", Type: "application-bundle", Path: filepath.ToSlash(bundlePath(plan, target)),
		Producer: producer, Target: artifactTarget(&target),
	}
	bundle.ID = artifactIdentity(bundle)
	return planned("bundle.assemble", &target, needs, []Artifact{binary, platform, assets}, []Artifact{bundle})
}

func distributionStages(
	plan *Plan,
	target Target,
	runnable Artifact,
	runnableProducer string,
	requestedFormats []string,
	planned func(string, *Target, []string, []Artifact, []Artifact) Stage,
	skipped func(string, *Target, []string, string) Stage,
) ([]Stage, []Artifact, []string) {
	signing := plan.Goal == "sign" || plan.Goal == "notarize"
	bundleSign := skipped("bundle.sign", &target, []string{runnableProducer}, "signing was not requested")
	if signing && (target.Platform == "darwin" || target.Platform == "ios" || target.Platform == "windows") {
		bundleSign = planned("bundle.sign", &target, []string{runnableProducer}, []Artifact{runnable}, nil)
	} else if signing {
		bundleSign.Reason = "target does not sign its runnable artifact before packaging"
	}
	stages := []Stage{bundleSign}

	formats := packageFormats(target.Platform, requestedFormats)
	finalArtifacts := make([]Artifact, 0, len(formats))
	finalNeeds := make([]string, 0, len(formats))
	for _, format := range formats {
		instance := formatStageInstance("package.create", target, format)
		output := packageArtifact(plan, target, format, instance)
		needs := []string{runnableProducer, bundleSign.Reference()}
		create := skipped("package.create", &target, needs, "not selected by the build goal")
		create.Instance = instance
		if plan.Goal != "build" {
			create = planned("package.create", &target, needs, []Artifact{runnable}, []Artifact{output})
			create.Instance = instance
			create.Outputs[0].Producer = instance
		}
		stages = append(stages, create)

		signInstance := formatStageInstance("package.sign", target, format)
		packageSign := skipped("package.sign", &target, []string{create.Reference()}, "signing was not requested")
		packageSign.Instance = signInstance
		if signing && signablePackageFormat(target.Platform, format) {
			packageSign = planned("package.sign", &target, []string{create.Reference()}, []Artifact{output}, nil)
			packageSign.Instance = signInstance
		} else if signing {
			packageSign.Reason = "package format does not use a separate signing step"
		}
		stages = append(stages, packageSign)

		notarizeInstance := formatStageInstance("package.notarize", target, format)
		notarize := skipped(
			"package.notarize",
			&target,
			[]string{create.Reference(), packageSign.Reference(), bundleSign.Reference()},
			"notarization was not requested",
		)
		notarize.Instance = notarizeInstance
		if plan.Goal == "notarize" && target.Platform == "darwin" {
			notarize = planned(
				"package.notarize",
				&target,
				[]string{create.Reference(), packageSign.Reference(), bundleSign.Reference()},
				[]Artifact{runnable, output},
				nil,
			)
			notarize.Instance = notarizeInstance
		}
		stages = append(stages, notarize)

		if plan.Goal == "build" {
			continue
		}
		finalArtifacts = append(finalArtifacts, output)
		finalNeeds = append(finalNeeds, create.Reference())
		if packageSign.Status == "planned" {
			finalNeeds = append(finalNeeds, packageSign.Reference())
		}
		if notarize.Status == "planned" {
			finalNeeds = append(finalNeeds, notarize.Reference())
		}
	}

	if plan.Goal == "build" {
		return stages, []Artifact{runnable}, []string{runnableProducer}
	}
	return stages, finalArtifacts, finalNeeds
}

func packageFormats(platform string, requested []string) []string {
	if len(requested) > 0 {
		return append([]string(nil), requested...)
	}
	switch platform {
	case "windows":
		return []string{"nsis"}
	case "darwin":
		return []string{"zip"}
	case "linux":
		return []string{"deb", "rpm", "archlinux"}
	case "android":
		return []string{"apk"}
	case "ios":
		return []string{"ipa"}
	default:
		return nil
	}
}

func formatStageInstance(id string, target Target, format string) string {
	return id + "[" + targetKey(target) + "/" + format + "]"
}

func packageArtifact(plan *Plan, target Target, format, producer string) Artifact {
	artifact := Artifact{
		Name:     "package",
		Type:     "distribution-package",
		Path:     filepath.ToSlash(packagePath(plan, target, format)),
		Producer: producer,
		Target: &ArtifactTarget{
			Platform: target.Platform,
			Arch:     target.Arch,
			Format:   format,
		},
	}
	artifact.ID = artifactIdentity(artifact)
	return artifact
}

func packagePath(plan *Plan, target Target, format string) string {
	directory := plan.Project.Output
	if len(plan.Targets) > 1 {
		directory = filepath.Join(directory, target.Platform, target.Arch)
	}
	name := plan.Project.BinaryName
	switch format {
	case "nsis":
		return filepath.Join(directory, name+"-"+strings.ToUpper(target.Arch)+"-installer.exe")
	case "archlinux":
		return filepath.Join(directory, name+".pkg.tar.zst")
	case "appimage":
		return filepath.Join(directory, name+"-"+target.Arch+".AppImage")
	case "msix":
		return filepath.Join(directory, name+"-"+target.Arch+".msix")
	default:
		return filepath.Join(directory, name+"."+format)
	}
}

func signablePackageFormat(platform, format string) bool {
	return (platform == "windows" && (format == "nsis" || format == "msix")) ||
		(platform == "linux" && (format == "deb" || format == "rpm"))
}

func bundlePath(plan *Plan, target Target) string {
	name := plan.Project.BinaryName + ".app"
	switch target.Platform {
	case "windows":
		name = plan.Project.BinaryName + ".windows"
	case "linux":
		name = plan.Project.BinaryName + ".AppDir"
	case "android":
		name = plan.Project.BinaryName + ".android"
	}
	if len(plan.Targets) == 1 {
		return filepath.Join(plan.Project.Output, name)
	}
	return filepath.Join(plan.Project.Output, target.Platform, target.Arch, name)
}

func binaryPath(plan *Plan, target Target) string {
	filename := plan.Project.BinaryName
	if target.Platform == "windows" && !strings.EqualFold(filepath.Ext(filename), ".exe") {
		filename += ".exe"
	}
	if target.Platform == "android" {
		filename = "libwails.so"
	}
	if target.Platform == "ios" {
		filename += ".a"
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
