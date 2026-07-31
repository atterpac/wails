package buildsystem

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecuteRunsBuiltinsHooksAndVerifiesArtifacts(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join("bin", "application")
	marker := filepath.Join(root, "before.marker")
	plan := &Plan{
		Project: Project{Root: root},
		Stages: []Stage{
			{
				ID:             "project.resolve",
				Implementation: "wails/project.resolve",
				Status:         "planned",
			},
			{
				ID:             "native.compile",
				Implementation: "wails/native.compile",
				Needs:          []string{"project.resolve"},
				Status:         "planned",
				Before: []Hook{{
					Command: []string{os.Args[0], "-test.run=TestBuildsystemHelperProcess", "--"},
					Environment: map[string]string{
						"GO_WANT_BUILDSYSTEM_HELPER": "1",
						"TARGET_FILE":                marker,
					},
				}},
				After: []Hook{{
					Command: []string{os.Args[0], "-test.run=TestBuildsystemHelperProcess", "--"},
					Environment: map[string]string{
						"GO_WANT_BUILDSYSTEM_HELPER": "1",
						"TARGET_FILE":                "${artifacts.binary}.after",
					},
				}},
				Outputs: []Artifact{{Name: "binary", Type: "native-binary", Path: output}},
			},
		},
	}

	var events []string
	err := Execute(context.Background(), plan, ExecuteOptions{
		Builtins: map[string]StageFunc{
			"project.resolve": func(context.Context, *StageContext) error { return nil },
			"native.compile": func(_ context.Context, stage *StageContext) error {
				path := resolvePath(stage.Plan.Project.Root, stage.Stage.Outputs[0].Path)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				return os.WriteFile(path, []byte("binary"), 0o755)
			},
		},
		OnStage: func(stage Stage, status string) {
			events = append(events, stage.ID+":"+status)
		},
	})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(root, output))
	assert.FileExists(t, filepath.Join(root, output+".after"))
	assert.FileExists(t, marker)
	assert.Equal(t, []string{
		"project.resolve:running",
		"project.resolve:completed",
		"native.compile:running",
		"native.compile:completed",
	}, events)
}

func TestExecuteRunsReplacementAndExpandsArtifactPath(t *testing.T) {
	root := t.TempDir()
	plan := &Plan{
		Project: Project{Root: root},
		Stages: []Stage{{
			ID:             "native.compile",
			Implementation: "command",
			Status:         "planned",
			Outputs:        []Artifact{{Name: "binary", Type: "native-binary", Path: "dist/custom"}},
			Replacement: &Command{
				Command: []string{os.Args[0], "-test.run=TestBuildsystemHelperProcess", "--"},
				Environment: map[string]string{
					"GO_WANT_BUILDSYSTEM_HELPER": "1",
					"TARGET_FILE":                "${project.root}/dist/custom",
				},
				Produces: map[string]string{"binary": "dist/custom"},
			},
		}},
	}

	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{}))
	assert.FileExists(t, filepath.Join(root, "dist", "custom"))
}

func TestExecuteRejectsInvalidRange(t *testing.T) {
	plan := &Plan{Stages: []Stage{
		{ID: "one"},
		{ID: "two"},
	}}

	err := Execute(context.Background(), plan, ExecuteOptions{From: "two", Until: "one"})
	require.EqualError(t, err, `--from stage "two" comes after --until stage "one"`)

	err = Execute(context.Background(), plan, ExecuteOptions{Step: "missing"})
	require.EqualError(t, err, `unknown build stage "missing"`)
}

func TestExecuteReportsMissingOutput(t *testing.T) {
	root := t.TempDir()
	plan := &Plan{
		Project: Project{Root: root},
		Stages: []Stage{{
			ID:      "native.compile",
			Status:  "planned",
			Outputs: []Artifact{{Name: "binary", Path: "bin/missing"}},
		}},
	}

	err := Execute(context.Background(), plan, ExecuteOptions{
		Builtins: map[string]StageFunc{
			"native.compile": func(context.Context, *StageContext) error { return nil },
		},
	})
	require.ErrorContains(t, err, `stage native.compile did not produce artifact "binary"`)
}

func TestBuildsystemHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_BUILDSYSTEM_HELPER") != "1" {
		return
	}
	path := os.Getenv("TARGET_FILE")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(path, []byte("generated"), 0o644); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}
