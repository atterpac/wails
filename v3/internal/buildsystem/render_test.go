package buildsystem

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanRenderers(t *testing.T) {
	plan, err := Resolve(Request{
		ProjectRoot: t.TempDir(),
		Target:      "darwin",
		Arch:        "arm64",
	})
	require.NoError(t, err)

	var jsonOutput bytes.Buffer
	require.NoError(t, WriteJSON(&jsonOutput, plan))
	var decoded Plan
	require.NoError(t, json.Unmarshal(jsonOutput.Bytes(), &decoded))
	assert.Equal(t, plan.Targets, decoded.Targets)
	assert.Equal(t, plan.Stages, decoded.Stages)

	var textOutput bytes.Buffer
	require.NoError(t, WriteText(&textOutput, plan))
	assert.Contains(t, textOutput.String(), "Build plan: 1 target(s) (production)")
	assert.Contains(t, textOutput.String(), "darwin/arm64 [production]")
	assert.Contains(t, textOutput.String(), "native.compile")
	assert.Contains(t, textOutput.String(), "single-architecture build")
}

func TestStageInspectionRenderers(t *testing.T) {
	plan, err := Resolve(Request{
		ProjectRoot: t.TempDir(),
		Target:      "linux",
		Arch:        "amd64",
	})
	require.NoError(t, err)

	inspection, err := InspectStage(plan, "native.compile")
	require.NoError(t, err)
	inspection.Stage.Actions = []Action{{
		Kind:             ActionCommand,
		Description:      "Compile application",
		Status:           "planned",
		Command:          []string{"go", "build", "-ldflags=-w -s"},
		WorkingDirectory: inspection.Project.Root,
		Environment: map[string]string{
			"GOARCH": "amd64",
			"GOOS":   "linux",
		},
	}}
	assert.Equal(t, "native.compile", inspection.Stage.ID)
	assert.Equal(t, plan.Targets, inspection.Targets)

	var jsonOutput bytes.Buffer
	require.NoError(t, WriteStageJSON(&jsonOutput, inspection))
	var decoded StageInspection
	require.NoError(t, json.Unmarshal(jsonOutput.Bytes(), &decoded))
	assert.Equal(t, "native.compile", decoded.Stage.ID)
	assert.Equal(t, []string{"frontend.build", "platform.generate[linux/amd64]"}, decoded.Stage.Needs)
	assert.Equal(t, inspection.Stage.Actions, decoded.Stage.Actions)

	var textOutput bytes.Buffer
	require.NoError(t, WriteStageText(&textOutput, inspection))
	assert.Contains(t, textOutput.String(), "Build step:     native.compile[linux/amd64]")
	assert.Contains(t, textOutput.String(), "Public stage:   native.compile")
	assert.Contains(t, textOutput.String(), "Implementation: wails/native.compile")
	assert.Contains(t, textOutput.String(), "Needs:          frontend.build, platform.generate[linux/amd64]")
	assert.Contains(t, textOutput.String(), "Execution:")
	assert.Contains(t, textOutput.String(), `"go" "build" "-ldflags=-w -s"`)
	assert.Contains(t, textOutput.String(), "GOARCH=amd64")
	assert.Contains(t, textOutput.String(), "frontend (frontend-distribution): frontend/dist")
	assert.Contains(t, textOutput.String(), "binary[linux/amd64] (native-binary): bin/")
	assert.Contains(t, textOutput.String(), "producer: native.compile")

	_, err = InspectStage(plan, "missing.stage")
	require.EqualError(t, err, `unknown build stage "missing.stage"`)
}
