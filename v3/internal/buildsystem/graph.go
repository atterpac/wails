package buildsystem

import (
	"fmt"
	"strings"
)

// Reference returns the resolved graph identity for a stage instance. ID is
// the stable public extension point; Instance identifies one matrix expansion.
func (stage Stage) Reference() string {
	if stage.Instance != "" {
		return stage.Instance
	}
	return stage.ID
}

// Reference returns the stable identity used to connect an artifact producer
// to its consumers. Target qualifiers prevent matrix outputs from colliding.
func (artifact Artifact) Reference() string {
	if artifact.ID != "" {
		return artifact.ID
	}
	return artifactIdentity(artifact)
}

func artifactIdentity(artifact Artifact) string {
	if artifact.Target == nil {
		return artifact.Name
	}
	qualifiers := make([]string, 0, 3)
	for _, value := range []string{
		artifact.Target.Platform,
		artifact.Target.Arch,
		artifact.Target.Format,
	} {
		if value != "" {
			qualifiers = append(qualifiers, value)
		}
	}
	if len(qualifiers) == 0 {
		return artifact.Name
	}
	return artifact.Name + "[" + strings.Join(qualifiers, "/") + "]"
}

func registerArtifacts(registry map[string]Artifact, artifacts []Artifact) {
	for _, artifact := range artifacts {
		reference := artifact.Reference()
		registry[reference] = artifact

		ambiguous := false
		for key, candidate := range registry {
			if key != candidate.Reference() || candidate.Name != artifact.Name {
				continue
			}
			if candidate.Reference() != reference {
				ambiguous = true
				break
			}
		}
		if ambiguous {
			delete(registry, artifact.Name)
			continue
		}
		registry[artifact.Name] = artifact
		if artifact.Producer != "" {
			registry[artifact.Producer+"."+artifact.Name] = artifact
			if base, _, qualified := strings.Cut(artifact.Producer, "["); qualified {
				registry[base+"."+artifact.Name] = artifact
			}
		}
	}
}

func normaliseArtifacts(plan *Plan) {
	for stageIndex := range plan.Stages {
		stage := &plan.Stages[stageIndex]
		if stage.Instance == "" {
			stage.Instance = stage.ID
		}
		for inputIndex := range stage.Inputs {
			input := &stage.Inputs[inputIndex]
			if input.ID == "" {
				input.ID = artifactIdentity(*input)
			}
		}
		for outputIndex := range stage.Outputs {
			output := &stage.Outputs[outputIndex]
			if output.Producer == "" {
				output.Producer = stage.Reference()
			}
			if output.ID == "" {
				output.ID = artifactIdentity(*output)
			}
		}
	}
}

// Validate checks the public graph and artifact contracts before inspection or
// execution. It intentionally does not require every input to be produced by a
// stage because explicit pipelines may accept user-supplied source artifacts.
func Validate(plan *Plan) error {
	if plan == nil {
		return fmt.Errorf("build plan is nil")
	}
	normaliseArtifacts(plan)

	stages := make(map[string]int, len(plan.Stages))
	for index, stage := range plan.Stages {
		if stage.ID == "" {
			return fmt.Errorf("build stage at index %d has no ID", index)
		}
		if stage.Reference() == "" {
			return fmt.Errorf("build stage at index %d has no instance", index)
		}
		if _, exists := stages[stage.Reference()]; exists {
			return fmt.Errorf("build plan contains duplicate stage %q", stage.Reference())
		}
		stages[stage.Reference()] = index
	}
	for _, stage := range plan.Stages {
		for _, dependency := range stage.Needs {
			if _, exists := stages[dependency]; !exists {
				return fmt.Errorf("stage %q requires unknown stage %q", stage.Reference(), dependency)
			}
			if dependency == stage.Reference() {
				return fmt.Errorf("stage %q depends on itself", stage.Reference())
			}
		}
	}
	if err := validateAcyclic(plan.Stages, stages); err != nil {
		return err
	}

	producers := make(map[string]string)
	paths := make(map[string]string)
	for _, stage := range plan.Stages {
		for _, output := range stage.Outputs {
			if output.Reference() == "" {
				return fmt.Errorf("stage %q contains an output artifact without an identity", stage.Reference())
			}
			if output.Producer != stage.Reference() {
				return fmt.Errorf(
					"stage %q declares artifact %q with producer %q",
					stage.Reference(),
					output.Reference(),
					output.Producer,
				)
			}
			if producer, exists := producers[output.Reference()]; exists {
				return fmt.Errorf(
					"artifact %q is produced by both %q and %q",
					output.Reference(),
					producer,
					stage.Reference(),
				)
			}
			producers[output.Reference()] = stage.Reference()
			if output.Path != "" {
				if artifactID, exists := paths[output.Path]; exists {
					return fmt.Errorf(
						"artifacts %q and %q share output path %q",
						artifactID,
						output.Reference(),
						output.Path,
					)
				}
				paths[output.Path] = output.Reference()
			}
		}
	}
	for _, stage := range plan.Stages {
		for _, input := range stage.Inputs {
			producer, exists := producers[input.Reference()]
			if !exists {
				continue
			}
			if !dependsOn(stage.Reference(), producer, plan.Stages, stages, make(map[string]bool)) {
				return fmt.Errorf(
					"stage %q consumes artifact %q from %q without depending on it",
					stage.Reference(),
					input.Reference(),
					producer,
				)
			}
		}
	}
	return nil
}

func validateAcyclic(stages []Stage, indexes map[string]int) error {
	states := make(map[string]uint8, len(stages))
	var visit func(string) error
	visit = func(id string) error {
		switch states[id] {
		case 1:
			return fmt.Errorf("build stage dependency cycle detected involving %q", id)
		case 2:
			return nil
		}
		states[id] = 1
		for _, dependency := range stages[indexes[id]].Needs {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		states[id] = 2
		return nil
	}
	for _, stage := range stages {
		if err := visit(stage.Reference()); err != nil {
			return err
		}
	}
	return nil
}

func dependsOn(
	stageID string,
	producerID string,
	stages []Stage,
	indexes map[string]int,
	visited map[string]bool,
) bool {
	if visited[stageID] {
		return false
	}
	visited[stageID] = true
	for _, dependency := range stages[indexes[stageID]].Needs {
		if dependency == producerID || dependsOn(dependency, producerID, stages, indexes, visited) {
			return true
		}
	}
	return false
}
