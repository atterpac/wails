package buildsystem

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const executionReportVersion = 1

type ExecutionReport struct {
	Version     int                    `json:"version"`
	PlanVersion string                 `json:"planVersion"`
	ID          string                 `json:"id"`
	Goal        string                 `json:"goal"`
	Mode        string                 `json:"mode"`
	ProjectRoot string                 `json:"projectRoot"`
	ReportPath  string                 `json:"reportPath"`
	CachePath   string                 `json:"cachePath,omitempty"`
	Resumed     bool                   `json:"resumed"`
	Parallel    bool                   `json:"parallel"`
	StartedAt   time.Time              `json:"startedAt"`
	FinishedAt  *time.Time             `json:"finishedAt,omitempty"`
	Duration    string                 `json:"duration,omitempty"`
	Status      string                 `json:"status"`
	Error       string                 `json:"error,omitempty"`
	Stages      []StageExecutionReport `json:"stages"`
}

type StageExecutionReport struct {
	ID          string                    `json:"id"`
	Instance    string                    `json:"instance"`
	Status      string                    `json:"status"`
	Fingerprint string                    `json:"fingerprint,omitempty"`
	Cache       string                    `json:"cache"`
	StartedAt   *time.Time                `json:"startedAt,omitempty"`
	FinishedAt  *time.Time                `json:"finishedAt,omitempty"`
	Duration    string                    `json:"duration,omitempty"`
	Error       string                    `json:"error,omitempty"`
	Outputs     []ArtifactExecutionReport `json:"outputs,omitempty"`
	Actions     []ActionExecutionReport   `json:"actions,omitempty"`
}

type ArtifactExecutionReport struct {
	Artifact Artifact `json:"artifact"`
	Exists   bool     `json:"exists"`
	Size     int64    `json:"size,omitempty"`
	SHA256   string   `json:"sha256,omitempty"`
}

type ActionExecutionReport struct {
	Index       int        `json:"index"`
	Kind        ActionKind `json:"kind"`
	Description string     `json:"description,omitempty"`
	Status      string     `json:"status"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Duration    string     `json:"duration,omitempty"`
	Error       string     `json:"error,omitempty"`
}

type executionRuntime struct {
	cache        *cacheStore
	cacheEnabled bool
	report       *reportRecorder
	previous     map[string]StageExecutionReport
	resume       bool
}

type reportRecorder struct {
	mu      sync.Mutex
	path    string
	report  ExecutionReport
	indexes map[string]int
	err     error
}

func newExecutionRuntime(plan *Plan, options ExecuteOptions, selected map[string]bool) (*executionRuntime, error) {
	if plan.Project.Root == "" && options.ReportPath == "" {
		return &executionRuntime{}, nil
	}
	reportPath := options.ReportPath
	if reportPath == "" {
		reportPath = filepath.Join(plan.Project.Root, ".wails", "build", "reports", "latest.json")
	} else if !filepath.IsAbs(reportPath) {
		reportPath = resolvePath(plan.Project.Root, reportPath)
	}
	var previous map[string]StageExecutionReport
	if options.Resume {
		loaded, err := readExecutionReport(reportPath)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read resume report: %w", err)
		}
		previous = make(map[string]StageExecutionReport)
		if loaded != nil && loaded.PlanVersion == plan.Version {
			for _, stage := range loaded.Stages {
				previous[stage.Instance] = stage
			}
		}
	}
	runtime := &executionRuntime{previous: previous, resume: options.Resume, cacheEnabled: !options.DisableCache}
	cachePath := ""
	cacheRequired := options.Resume
	for _, stage := range plan.Stages {
		cacheRequired = cacheRequired || selected[stage.Reference()] && stage.Cache.Enabled && !options.DisableCache
	}
	if cacheRequired {
		cache, err := newCacheStore(plan, options.CacheDirectory, reportPath)
		if err != nil {
			return nil, err
		}
		runtime.cache = cache
		if runtime.cacheEnabled {
			cachePath = cache.root
		}
	}
	now := time.Now().UTC()
	report := ExecutionReport{
		Version: executionReportVersion, PlanVersion: plan.Version,
		ID: fmt.Sprintf("%d-%d", now.UnixNano(), os.Getpid()), Goal: plan.Goal, Mode: plan.Mode,
		ProjectRoot: plan.Project.Root, ReportPath: reportPath, CachePath: cachePath,
		Resumed: options.Resume, Parallel: options.Parallel, StartedAt: now, Status: "running",
	}
	indexes := make(map[string]int)
	for _, stage := range plan.Stages {
		if !selected[stage.Reference()] {
			continue
		}
		indexes[stage.Reference()] = len(report.Stages)
		entry := StageExecutionReport{ID: stage.ID, Instance: stage.Reference(), Status: "pending", Cache: "disabled"}
		for _, output := range stage.Outputs {
			entry.Outputs = append(entry.Outputs, ArtifactExecutionReport{Artifact: output})
		}
		for index, action := range stage.Actions {
			status := "pending"
			if action.Status == "skipped" {
				status = "skipped"
			}
			entry.Actions = append(entry.Actions, ActionExecutionReport{Index: index, Kind: action.Kind, Description: action.Description, Status: status})
		}
		report.Stages = append(report.Stages, entry)
	}
	runtime.report = &reportRecorder{path: reportPath, report: report, indexes: indexes}
	if err := runtime.report.write(); err != nil {
		return nil, fmt.Errorf("write initial execution report: %w", err)
	}
	return runtime, nil
}

func readExecutionReport(path string) (*ExecutionReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var report ExecutionReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

func (runtime *executionRuntime) canResume(stage Stage, fingerprint string) bool {
	previous, ok := runtime.previous[stage.Reference()]
	if !runtime.resume || !ok || previous.Status != "completed" || previous.Fingerprint != fingerprint {
		return false
	}
	return verifyOutputs(stageRoot(runtime, stage), stage) == nil
}

func stageRoot(runtime *executionRuntime, _ Stage) string {
	if runtime.cache != nil {
		return runtime.cache.projectRoot
	}
	return ""
}

func (recorder *reportRecorder) stageStarted(stage Stage, fingerprint, cacheDecision string) {
	recorder.update(func(report *ExecutionReport) {
		entry := &report.Stages[recorder.indexes[stage.Reference()]]
		now := time.Now().UTC()
		entry.StartedAt = &now
		entry.Status = "running"
		entry.Fingerprint = fingerprint
		entry.Cache = cacheDecision
	})
}

func (recorder *reportRecorder) stageFinished(stage Stage, status, cacheDecision string, err error) {
	provenance := collectOutputProvenance(recorder.report.ProjectRoot, stage.Outputs)
	recorder.update(func(report *ExecutionReport) {
		entry := &report.Stages[recorder.indexes[stage.Reference()]]
		now := time.Now().UTC()
		entry.FinishedAt = &now
		entry.Status = status
		entry.Cache = cacheDecision
		entry.Outputs = provenance
		if entry.StartedAt != nil {
			entry.Duration = now.Sub(*entry.StartedAt).String()
		}
		if err != nil {
			entry.Error = err.Error()
		}
	})
}

func collectOutputProvenance(root string, artifacts []Artifact) []ArtifactExecutionReport {
	result := make([]ArtifactExecutionReport, 0, len(artifacts))
	for _, artifact := range artifacts {
		entry := ArtifactExecutionReport{Artifact: artifact}
		if artifact.Path == "" {
			result = append(result, entry)
			continue
		}
		path := resolvePath(root, artifact.Path)
		if _, err := os.Lstat(path); err != nil {
			result = append(result, entry)
			continue
		}
		entry.Exists = true
		entry.SHA256, _ = hashFilesystemPath(path)
		entry.Size, _ = filesystemSize(path)
		result = append(result, entry)
	}
	return result
}

func (recorder *reportRecorder) actionStarted(stage Stage, index int) {
	recorder.update(func(report *ExecutionReport) {
		entry := &report.Stages[recorder.indexes[stage.Reference()]]
		if index >= len(entry.Actions) {
			return
		}
		now := time.Now().UTC()
		entry.Actions[index].StartedAt = &now
		entry.Actions[index].Status = "running"
	})
}

func (recorder *reportRecorder) actionFinished(stage Stage, index int, status string, err error) {
	recorder.update(func(report *ExecutionReport) {
		entry := &report.Stages[recorder.indexes[stage.Reference()]]
		if index >= len(entry.Actions) {
			return
		}
		action := &entry.Actions[index]
		now := time.Now().UTC()
		action.FinishedAt = &now
		action.Status = status
		if action.StartedAt != nil {
			action.Duration = now.Sub(*action.StartedAt).String()
		}
		if err != nil {
			action.Error = err.Error()
		}
	})
}

func (recorder *reportRecorder) finish(err error) error {
	recorder.mu.Lock()
	now := time.Now().UTC()
	recorder.report.FinishedAt = &now
	recorder.report.Duration = now.Sub(recorder.report.StartedAt).String()
	if err != nil {
		recorder.report.Status = "failed"
		recorder.report.Error = err.Error()
	} else {
		recorder.report.Status = "completed"
	}
	for index := range recorder.report.Stages {
		if recorder.report.Stages[index].Status == "pending" {
			recorder.report.Stages[index].Status = "not-run"
		}
	}
	writeErr := recorder.writeLocked()
	result := errors.Join(recorder.err, writeErr)
	recorder.mu.Unlock()
	return result
}

func (recorder *reportRecorder) update(update func(*ExecutionReport)) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	update(&recorder.report)
	if err := recorder.writeLocked(); err != nil {
		recorder.err = errors.Join(recorder.err, err)
	}
}

func (recorder *reportRecorder) write() error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.writeLocked()
}

func (recorder *reportRecorder) writeLocked() error {
	if err := os.MkdirAll(filepath.Dir(recorder.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(recorder.report, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(recorder.path), ".report-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, recorder.path); err == nil {
		return nil
	}
	if err := os.Remove(recorder.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(temporaryPath, recorder.path)
}
