package buildsystem

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

func TestExecuteRunsBuildScopedHookOnceAcrossParallelStages(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "hook-runs")
	hook := Hook{
		ID: "native.compile.before.0", Name: "prepare", Scope: "build", Status: "planned",
		Command: []string{os.Args[0], "-test.run=TestBuildsystemHelperProcess", "--"},
		Environment: map[string]string{
			"GO_WANT_BUILDSYSTEM_HELPER": "1", "TARGET_FILE": marker, "HELPER_APPEND": "1",
		},
	}
	plan := &Plan{Project: Project{Root: root}, Stages: []Stage{
		{ID: "native.compile", Instance: "native.compile[linux/amd64]", Status: "planned", Before: []Hook{hook}},
		{ID: "native.compile", Instance: "native.compile[linux/arm64]", Status: "planned", Before: []Hook{hook}},
	}}
	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{
		Parallel: true,
		Builtins: map[string]StageFunc{"native.compile": func(context.Context, *StageContext) error { return nil }},
	}))
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "run\n", string(data))
}

func TestExecuteHookValidatesInputsPublishesOutputsAndContinuesFailures(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "source"), []byte("input"), 0o644))
	var stderr bytes.Buffer
	plan := &Plan{Project: Project{Root: root}, Stages: []Stage{{
		ID: "custom", Status: "planned",
		Before: []Hook{{
			ID: "custom.before.0", Name: "generate", Scope: "stage", Status: "planned",
			Command: []string{os.Args[0], "-test.run=TestBuildsystemHelperProcess", "--"},
			Environment: map[string]string{
				"GO_WANT_BUILDSYSTEM_HELPER": "1", "TARGET_FILE": "${project.root}/generated", "HELPER_COPY_INPUT": "1",
			},
			Inputs:  map[string]string{"source": "${project.root}/source"},
			Outputs: map[string]string{"generated": "generated"},
		}, {
			ID: "custom.before.1", Name: "nonfatal", Scope: "stage", Status: "planned", OnFailure: "continue",
			Command:     []string{os.Args[0], "-test.run=TestBuildsystemHelperProcess", "--"},
			Environment: map[string]string{"GO_WANT_BUILDSYSTEM_HELPER": "1", "HELPER_FAIL": "1"},
		}},
	}}}
	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{
		Stderr:   &stderr,
		Builtins: map[string]StageFunc{"custom": func(context.Context, *StageContext) error { return nil }},
	}))
	data, err := os.ReadFile(filepath.Join(root, "generated"))
	require.NoError(t, err)
	assert.Equal(t, "input", string(data))
	assert.Contains(t, stderr.String(), "allowed to continue")
}

func TestExecuteRunsResolvedActions(t *testing.T) {
	root := t.TempDir()
	actions := []Action{
		{
			Kind:        ActionMkdir,
			Description: "Create action workspace",
			Path:        "${project.root}/work",
		},
		{
			Kind:        ActionCommand,
			Description: "Generate source file",
			Command:     []string{os.Args[0], "-test.run=TestBuildsystemHelperProcess", "--"},
			Environment: map[string]string{
				"GO_WANT_BUILDSYSTEM_HELPER": "1",
				"TARGET_FILE":                "${project.root}/work/source",
			},
		},
		{
			Kind:        ActionCopy,
			Description: "Copy generated file",
			Source:      "${project.root}/work/source",
			Destination: "${project.root}/work/result",
		},
		{
			Kind:        ActionVerify,
			Description: "Verify copied file",
			Path:        "${project.root}/work/result",
		},
		{
			Kind:        ActionRemove,
			Description: "Remove temporary source",
			Path:        "${project.root}/work/source",
		},
	}
	plan := &Plan{
		Project: Project{Root: root},
		Stages: []Stage{{
			ID:      "native.compile",
			Status:  "planned",
			Actions: actions,
			Outputs: []Artifact{{Name: "binary", Path: "work/result"}},
		}},
	}

	var executed []Action
	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{
		OnAction: func(_ Stage, action Action, status string) {
			if status == "completed" {
				executed = append(executed, action)
			}
		},
	}))
	assert.Equal(t, actions, executed)
	assert.FileExists(t, filepath.Join(root, "work", "result"))
	assert.NoFileExists(t, filepath.Join(root, "work", "source"))
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

func TestExecuteUsesDependencyOrderInsteadOfStorageOrder(t *testing.T) {
	plan := &Plan{Stages: []Stage{
		{ID: "collect", Needs: []string{"compile"}, Status: "planned"},
		{ID: "compile", Needs: []string{"generate"}, Status: "planned"},
		{ID: "generate", Status: "planned"},
	}}
	var executed []string

	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{
		Builtins: map[string]StageFunc{
			"generate": func(context.Context, *StageContext) error { return nil },
			"compile":  func(context.Context, *StageContext) error { return nil },
			"collect":  func(context.Context, *StageContext) error { return nil },
		},
		OnStage: func(stage Stage, status string) {
			if status == "completed" {
				executed = append(executed, stage.ID)
			}
		},
	}))
	assert.Equal(t, []string{"generate", "compile", "collect"}, executed)
}

func TestExecuteParallelRunsIndependentStagesConcurrently(t *testing.T) {
	plan := &Plan{Stages: []Stage{
		{ID: "resolve", Status: "planned"},
		{ID: "first", Needs: []string{"resolve"}, Status: "planned"},
		{ID: "second", Needs: []string{"resolve"}, Status: "planned"},
		{ID: "collect", Needs: []string{"first", "second"}, Status: "planned"},
	}}
	started := make(chan string, 2)
	release := make(chan struct{})
	var mu sync.Mutex
	var completed []string
	done := make(chan error, 1)
	go func() {
		done <- Execute(context.Background(), plan, ExecuteOptions{
			Parallel: true,
			Builtins: map[string]StageFunc{
				"resolve": func(context.Context, *StageContext) error { return nil },
				"first": func(context.Context, *StageContext) error {
					started <- "first"
					<-release
					return nil
				},
				"second": func(context.Context, *StageContext) error {
					started <- "second"
					<-release
					return nil
				},
				"collect": func(context.Context, *StageContext) error { return nil },
			},
			OnStage: func(stage Stage, status string) {
				if status == "completed" {
					mu.Lock()
					completed = append(completed, stage.Reference())
					mu.Unlock()
				}
			},
		})
	}()

	seen := map[string]bool{}
	for range 2 {
		select {
		case stage := <-started:
			seen[stage] = true
		case <-time.After(time.Second):
			t.Fatal("independent stages did not start concurrently")
		}
	}
	close(release)
	require.NoError(t, <-done)
	assert.Equal(t, map[string]bool{"first": true, "second": true}, seen)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "resolve", completed[0])
	assert.Equal(t, "collect", completed[len(completed)-1])
}

func TestExecuteParallelCancelsSiblingsAfterFailure(t *testing.T) {
	failure := errors.New("compile failed")
	plan := &Plan{Stages: []Stage{
		{ID: "fail", Status: "planned"},
		{ID: "wait", Status: "planned"},
		{ID: "downstream", Needs: []string{"wait"}, Status: "planned"},
	}}
	var started sync.WaitGroup
	started.Add(2)
	cancelled := make(chan struct{})
	downstreamRan := false

	err := Execute(context.Background(), plan, ExecuteOptions{
		Parallel: true,
		Builtins: map[string]StageFunc{
			"fail": func(context.Context, *StageContext) error {
				started.Done()
				started.Wait()
				return failure
			},
			"wait": func(ctx context.Context, _ *StageContext) error {
				started.Done()
				started.Wait()
				<-ctx.Done()
				close(cancelled)
				return ctx.Err()
			},
			"downstream": func(context.Context, *StageContext) error {
				downstreamRan = true
				return nil
			},
		},
	})
	require.ErrorIs(t, err, failure)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("sibling stage was not cancelled")
	}
	assert.False(t, downstreamRan)
}

func TestExecutionOrderUsesDependencyClosures(t *testing.T) {
	stages := []Stage{
		{ID: "resolve"},
		{ID: "assets", Needs: []string{"resolve"}},
		{ID: "bindings", Needs: []string{"resolve"}},
		{ID: "compile", Needs: []string{"assets", "bindings"}},
		{ID: "package", Needs: []string{"compile"}},
		{ID: "unrelated", Needs: []string{"resolve"}},
	}
	require.NoError(t, Validate(&Plan{Stages: stages}))

	order, _, err := executionOrder(stages, ExecuteOptions{Until: "compile"})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1, 2, 3}, order)

	order, _, err = executionOrder(stages, ExecuteOptions{From: "compile"})
	require.NoError(t, err)
	assert.Equal(t, []int{3, 4}, order)

	order, _, err = executionOrder(stages, ExecuteOptions{From: "assets", Until: "package"})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 3, 4}, order)
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

func TestExecuteRunsFinallyActionsAfterFailure(t *testing.T) {
	root := t.TempDir()
	temporary := filepath.Join(root, "temporary.syso")
	require.NoError(t, os.WriteFile(temporary, []byte("temporary"), 0o644))
	plan := &Plan{
		Project: Project{Root: root},
		Stages: []Stage{{
			ID:     "native.compile",
			Status: "planned",
			Actions: []Action{
				{
					Kind:    ActionCommand,
					Command: []string{os.Args[0], "-test.run=TestBuildsystemHelperProcess", "--"},
					Environment: map[string]string{
						"GO_WANT_BUILDSYSTEM_HELPER": "1",
						"HELPER_FAIL":                "1",
					},
				},
				{
					Kind:    ActionRemove,
					Path:    temporary,
					Finally: true,
				},
			},
		}},
	}

	err := Execute(context.Background(), plan, ExecuteOptions{})
	require.Error(t, err)
	assert.NoFileExists(t, temporary)
}

func TestFinalActionsRunWithoutCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cleanupRan := false
	stage := &StageContext{Stage: Stage{ID: "cleanup"}}
	err := executeFinalActions(ctx, stage, ExecuteOptions{
		callbackMu: &sync.Mutex{},
		InternalActions: map[string]ActionFunc{
			"cleanup": func(ctx context.Context, _ *StageContext, _ Action) error {
				require.NoError(t, ctx.Err())
				cleanupRan = true
				return nil
			},
		},
	}, []Action{{Kind: ActionInternal, Internal: "cleanup", Finally: true}})
	require.NoError(t, err)
	assert.True(t, cleanupRan)
}

func TestExecuteChecksRequiredTools(t *testing.T) {
	root := t.TempDir()
	plan := &Plan{
		Project: Project{Root: root},
		Stages: []Stage{{
			ID:     "toolchain.check",
			Status: "planned",
			Actions: []Action{{
				Kind: ActionCheckTool,
				Tool: os.Args[0],
			}},
		}},
	}
	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{}))

	plan.Stages[0].Actions[0].Tool = "wails-buildsystem-definitely-missing-tool"
	err := Execute(context.Background(), plan, ExecuteOptions{})
	require.EqualError(t, err, `stage toolchain.check: required tool "wails-buildsystem-definitely-missing-tool" was not found in PATH`)
}

func TestExecuteExpandsQualifiedArtifactReference(t *testing.T) {
	root := t.TempDir()
	target := &ArtifactTarget{Platform: "windows", Arch: "amd64"}
	plan := &Plan{
		Project: Project{Root: root},
		Stages: []Stage{
			{
				ID:      "windows.compile",
				Status:  "planned",
				Outputs: []Artifact{{Name: "binary", Path: "bin/app.exe", Target: target}},
			},
			{
				ID:     "report",
				Needs:  []string{"windows.compile"},
				Status: "planned",
				Inputs: []Artifact{{Name: "binary", Path: "bin/app.exe", Target: target}},
				Actions: []Action{{
					Kind:    ActionCommand,
					Command: []string{os.Args[0], "-test.run=TestBuildsystemHelperProcess", "--"},
					Environment: map[string]string{
						"GO_WANT_BUILDSYSTEM_HELPER": "1",
						"TARGET_FILE":                "${artifacts.binary[windows/amd64]}.report",
					},
				}},
			},
		},
	}

	require.NoError(t, Execute(context.Background(), plan, ExecuteOptions{
		Builtins: map[string]StageFunc{
			"windows.compile": func(_ context.Context, stage *StageContext) error {
				path := resolvePath(stage.Plan.Project.Root, stage.Stage.Outputs[0].Path)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				return os.WriteFile(path, []byte("binary"), 0o755)
			},
		},
	}))
	assert.FileExists(t, filepath.Join(root, "bin", "app.exe.report"))
}

func TestBuildsystemHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_BUILDSYSTEM_HELPER") != "1" {
		return
	}
	if os.Getenv("HELPER_FAIL") == "1" {
		os.Exit(4)
	}
	path := os.Getenv("TARGET_FILE")
	if os.Getenv("HELPER_COPY_INPUT") == "1" {
		data, err := os.ReadFile(os.Getenv("WAILS_HOOK_INPUT_SOURCE"))
		if err != nil || os.WriteFile(path, data, 0o644) != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		os.Exit(2)
	}
	if os.Getenv("HELPER_APPEND") == "1" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			os.Exit(3)
		}
		_, err = file.WriteString("run\n")
		_ = file.Close()
		if err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	if err := os.WriteFile(path, []byte("generated"), 0o644); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}
