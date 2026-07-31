package buildsystem

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArtifactReferenceIncludesTargetCoordinates(t *testing.T) {
	artifact := Artifact{
		Name: "package",
		Target: &ArtifactTarget{
			Platform: "linux",
			Arch:     "arm64",
			Format:   "deb",
		},
	}

	assert.Equal(t, "package[linux/arm64/deb]", artifact.Reference())
	artifact.ID = "release-package"
	assert.Equal(t, "release-package", artifact.Reference())
}

func TestValidateRejectsInvalidStageGraph(t *testing.T) {
	tests := []struct {
		name   string
		stages []Stage
		error  string
	}{
		{
			name: "duplicate stage",
			stages: []Stage{
				{ID: "compile"},
				{ID: "compile"},
			},
			error: `build plan contains duplicate stage "compile"`,
		},
		{
			name:   "unknown dependency",
			stages: []Stage{{ID: "compile", Needs: []string{"generate"}}},
			error:  `stage "compile" requires unknown stage "generate"`,
		},
		{
			name: "cycle",
			stages: []Stage{
				{ID: "generate", Needs: []string{"compile"}},
				{ID: "compile", Needs: []string{"generate"}},
			},
			error: `build stage dependency cycle detected involving "generate"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(&Plan{Stages: test.stages})
			require.EqualError(t, err, test.error)
		})
	}
}

func TestValidateRejectsArtifactIdentityCollision(t *testing.T) {
	plan := &Plan{Stages: []Stage{
		{ID: "first", Outputs: []Artifact{{Name: "binary"}}},
		{ID: "second", Outputs: []Artifact{{Name: "binary"}}},
	}}

	err := Validate(plan)
	require.EqualError(t, err, `artifact "binary" is produced by both "first" and "second"`)
}

func TestValidateRejectsArtifactOutputPathCollision(t *testing.T) {
	plan := &Plan{Stages: []Stage{
		{ID: "first", Outputs: []Artifact{{Name: "one", Path: "bin/shared"}}},
		{ID: "second", Outputs: []Artifact{{Name: "two", Path: "bin/shared"}}},
	}}

	err := Validate(plan)
	require.EqualError(t, err, `artifacts "one" and "two" share output path "bin/shared"`)
}

func TestValidateAcceptsTargetSpecificArtifactVariants(t *testing.T) {
	windows := &ArtifactTarget{Platform: "windows", Arch: "amd64"}
	linux := &ArtifactTarget{Platform: "linux", Arch: "amd64"}
	plan := &Plan{Stages: []Stage{
		{ID: "windows.compile", Outputs: []Artifact{{Name: "binary", Target: windows}}},
		{ID: "linux.compile", Outputs: []Artifact{{Name: "binary", Target: linux}}},
		{
			ID:    "collect",
			Needs: []string{"windows.compile", "linux.compile"},
			Inputs: []Artifact{
				{Name: "binary", Target: windows},
				{Name: "binary", Target: linux},
			},
		},
	}}

	require.NoError(t, Validate(plan))
	assert.Equal(t, "binary[windows/amd64]", plan.Stages[0].Outputs[0].ID)
	assert.Equal(t, "windows.compile", plan.Stages[0].Outputs[0].Producer)
	assert.Equal(t, "binary[linux/amd64]", plan.Stages[1].Outputs[0].ID)
}

func TestValidateRequiresArtifactProducerDependency(t *testing.T) {
	plan := &Plan{Stages: []Stage{
		{ID: "generate", Outputs: []Artifact{{Name: "source"}}},
		{ID: "compile", Inputs: []Artifact{{Name: "source"}}},
	}}

	err := Validate(plan)
	require.EqualError(
		t,
		err,
		`stage "compile" consumes artifact "source" from "generate" without depending on it`,
	)
}

func TestRegisterArtifactsRequiresQualifiedLookupWhenAmbiguous(t *testing.T) {
	registry := make(map[string]Artifact)
	registerArtifacts(registry, []Artifact{
		{Name: "binary", Target: &ArtifactTarget{Platform: "windows", Arch: "amd64"}},
		{Name: "binary", Target: &ArtifactTarget{Platform: "linux", Arch: "amd64"}},
	})

	_, hasUnqualified := registry["binary"]
	assert.False(t, hasUnqualified)
	assert.Contains(t, registry, "binary[windows/amd64]")
	assert.Contains(t, registry, "binary[linux/amd64]")
}
