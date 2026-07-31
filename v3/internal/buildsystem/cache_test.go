package buildsystem

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecuteStoresAndRestoresStageCache(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "source.go"), []byte("package main"), 0o644))
	output := filepath.Join(root, "dist", "application")
	plan := cacheTestPlan(root, output)
	cacheDirectory := filepath.Join(root, "local-cache")
	reportPath := filepath.Join(root, "run-report.json")

	var firstActions int
	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{
		CacheDirectory: cacheDirectory,
		ReportPath:     reportPath,
		OnAction: func(_ Stage, _ Action, status string) {
			if status == "completed" {
				firstActions++
			}
		},
	}))
	assert.Equal(t, 1, firstActions)
	assert.FileExists(t, output)

	require.NoError(t, os.Remove(output))
	var secondActions int
	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{
		CacheDirectory: cacheDirectory,
		ReportPath:     reportPath,
		OnAction: func(_ Stage, _ Action, status string) {
			if status == "completed" {
				secondActions++
			}
		},
	}))
	assert.Zero(t, secondActions)
	assert.FileExists(t, output)

	report, err := readExecutionReport(reportPath)
	require.NoError(t, err)
	require.Len(t, report.Stages, 1)
	assert.Equal(t, "hit", report.Stages[0].Cache)
	assert.NotEmpty(t, report.Stages[0].Fingerprint)
}

func TestStageCacheInvalidatesWhenProjectSourcesChange(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.go")
	require.NoError(t, os.WriteFile(source, []byte("package main"), 0o644))
	output := filepath.Join(root, "dist", "application")
	plan := cacheTestPlan(root, output)
	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{}))
	require.NoError(t, os.Remove(output))
	require.NoError(t, os.WriteFile(source, []byte("package main\n// changed"), 0o644))

	var actions int
	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{
		OnAction: func(_ Stage, _ Action, status string) {
			if status == "completed" {
				actions++
			}
		},
	}))
	assert.Equal(t, 1, actions)
	report, err := readExecutionReport(filepath.Join(root, ".wails", "build", "reports", "latest.json"))
	require.NoError(t, err)
	assert.Equal(t, "stored", report.Stages[0].Cache)
}

func TestCorruptCacheEntryFallsBackToStageExecution(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "source.go"), []byte("package main"), 0o644))
	output := filepath.Join(root, "dist", "application")
	plan := cacheTestPlan(root, output)
	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{}))
	reportPath := filepath.Join(root, ".wails", "build", "reports", "latest.json")
	report, err := readExecutionReport(reportPath)
	require.NoError(t, err)
	fingerprint := report.Stages[0].Fingerprint
	require.NoError(t, os.WriteFile(
		filepath.Join(root, ".wails", "build", "cache", fingerprint, "outputs", "0"),
		[]byte("corrupt"), 0o644,
	))
	require.NoError(t, os.Remove(output))

	var stderr bytes.Buffer
	var actions int
	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{
		Stderr: &stderr,
		OnAction: func(_ Stage, _ Action, status string) {
			if status == "completed" {
				actions++
			}
		},
	}))
	assert.Equal(t, 1, actions)
	assert.Contains(t, stderr.String(), "failed integrity verification")
}

func TestExecuteResumeUsesVerifiedCompletedOutputs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "source.go"), []byte("package main"), 0o644))
	output := filepath.Join(root, "dist", "application")
	plan := cacheTestPlan(root, output)
	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{}))

	var actions int
	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{
		Resume: true,
		OnAction: func(_ Stage, _ Action, status string) {
			if status == "completed" {
				actions++
			}
		},
	}))
	assert.Zero(t, actions)
	report, err := readExecutionReport(filepath.Join(root, ".wails", "build", "reports", "latest.json"))
	require.NoError(t, err)
	assert.True(t, report.Resumed)
	assert.Equal(t, "resumed", report.Stages[0].Cache)
}

func TestExecutionReportRecordsFailures(t *testing.T) {
	root := t.TempDir()
	plan := &Plan{Project: Project{Root: root}, Stages: []Stage{{
		ID: "compile", Status: "planned", Actions: []Action{{
			Kind: ActionCommand, Command: []string{os.Args[0], "-test.run=TestBuildsystemHelperProcess", "--"},
			Environment: map[string]string{"GO_WANT_BUILDSYSTEM_HELPER": "1", "HELPER_FAIL": "1"},
		}},
	}}}
	require.Error(t, Execute(context.Background(), plan, ExecuteOptions{}))
	report, err := readExecutionReport(filepath.Join(root, ".wails", "build", "reports", "latest.json"))
	require.NoError(t, err)
	assert.Equal(t, "failed", report.Status)
	require.Len(t, report.Stages, 1)
	assert.Equal(t, "failed", report.Stages[0].Status)
	require.Len(t, report.Stages[0].Actions, 1)
	assert.Equal(t, "failed", report.Stages[0].Actions[0].Status)
	assert.NotEmpty(t, report.Stages[0].Actions[0].Error)
}

func cacheTestPlan(root, output string) *Plan {
	return &Plan{
		Version: PlanVersion, Project: Project{Root: root, Output: "dist"}, Mode: "production", Goal: "build",
		Stages: []Stage{{
			ID: "compile", Status: "planned", Cache: CachePolicy{Enabled: true, Sources: []string{"${project.root}"}},
			Actions: []Action{{
				Kind: ActionCommand, Description: "Generate output",
				Command:     []string{os.Args[0], "-test.run=TestBuildsystemHelperProcess", "--"},
				Environment: map[string]string{"GO_WANT_BUILDSYSTEM_HELPER": "1", "TARGET_FILE": output},
			}},
			Outputs: []Artifact{{Name: "binary", Type: "native-binary", Path: filepath.ToSlash(filepath.Join("dist", "application"))}},
		}},
	}
}
