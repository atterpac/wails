package buildsystem

import (
	"context"
	"fmt"
)

type stageResult struct {
	index   int
	outputs []Artifact
	err     error
}

func executeParallel(
	ctx context.Context,
	plan *Plan,
	options ExecuteOptions,
	selected map[string]bool,
	artifacts map[string]Artifact,
) error {
	if len(selected) == 0 {
		return nil
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	inDegree := make(map[string]int, len(selected))
	dependents := make(map[string][]int, len(selected))
	for index, stage := range plan.Stages {
		if !selected[stage.Reference()] {
			continue
		}
		for _, dependency := range stage.Needs {
			if !selected[dependency] {
				continue
			}
			inDegree[stage.Reference()]++
			dependents[dependency] = append(dependents[dependency], index)
		}
	}

	ready := make([]int, 0, len(selected))
	for index, stage := range plan.Stages {
		if selected[stage.Reference()] && inDegree[stage.Reference()] == 0 {
			ready = append(ready, index)
		}
	}
	results := make(chan stageResult, len(selected))
	running := 0
	finished := 0
	var firstErr error
	ctxDone := runCtx.Done()

	launchReady := func() {
		if firstErr != nil {
			return
		}
		for _, index := range ready {
			stage := plan.Stages[index]
			snapshot := cloneArtifacts(artifacts)
			running++
			go func() {
				outputs, err := executeStage(runCtx, plan, stage, options, snapshot)
				results <- stageResult{index: index, outputs: outputs, err: err}
			}()
		}
		ready = ready[:0]
	}

	for finished < len(selected) {
		launchReady()
		if running == 0 {
			if firstErr != nil {
				return firstErr
			}
			return fmt.Errorf("parallel build scheduler stalled with %d unfinished stage(s)", len(selected)-finished)
		}

		select {
		case result := <-results:
			running--
			finished++
			stage := plan.Stages[result.index]
			if result.err != nil {
				if firstErr == nil {
					firstErr = result.err
					cancel(result.err)
					ctxDone = nil
				}
				continue
			}
			registerArtifacts(artifacts, result.outputs)
			if firstErr != nil {
				continue
			}
			for _, dependentIndex := range dependents[stage.Reference()] {
				dependent := plan.Stages[dependentIndex]
				inDegree[dependent.Reference()]--
				if inDegree[dependent.Reference()] == 0 {
					ready = append(ready, dependentIndex)
				}
			}
		case <-ctxDone:
			if firstErr == nil {
				firstErr = context.Cause(runCtx)
				cancel(firstErr)
			}
			ctxDone = nil
		}
	}
	return firstErr
}
