package buildsystem

// ResolveExpressions replaces project, target, and artifact expressions in all
// executable plan fields after the complete artifact graph and actions exist.
func ResolveExpressions(plan *Plan) {
	for stageIndex := range plan.Stages {
		stage := &plan.Stages[stageIndex]
		artifacts := make(map[string]Artifact)
		registerArtifacts(artifacts, stage.Inputs)
		registerArtifacts(artifacts, stage.Outputs)
		context := &StageContext{Plan: plan, Stage: *stage, Artifacts: artifacts}
		for actionIndex := range stage.Actions {
			action := &stage.Actions[actionIndex]
			action.Command = expandStrings(action.Command, context)
			action.ResolvedCommand = expandStrings(action.ResolvedCommand, context)
			action.WorkingDirectory = expand(action.WorkingDirectory, context)
			action.Environment = expandStringMap(action.Environment, context)
			action.Source = expand(action.Source, context)
			action.Destination = expand(action.Destination, context)
			action.Path = expand(action.Path, context)
			action.Parameters = expandStringMap(action.Parameters, context)
		}
		resolveHookExpressions(stage.Before, context)
		resolveHookExpressions(stage.After, context)
		if stage.Replacement != nil {
			stage.Replacement.Command = expandStrings(stage.Replacement.Command, context)
			stage.Replacement.Args = expandStrings(stage.Replacement.Args, context)
			stage.Replacement.WorkingDirectory = expand(stage.Replacement.WorkingDirectory, context)
			stage.Replacement.Environment = expandStringMap(stage.Replacement.Environment, context)
			stage.Replacement.Produces = expandStringMap(stage.Replacement.Produces, context)
		}
	}
}

func resolveHookExpressions(hooks []Hook, context *StageContext) {
	for index := range hooks {
		hooks[index].Command = expandStrings(hooks[index].Command, context)
		hooks[index].ResolvedCommand = expandStrings(hooks[index].ResolvedCommand, context)
		hooks[index].WorkingDirectory = expand(hooks[index].WorkingDirectory, context)
		hooks[index].Environment = expandStringMap(hooks[index].Environment, context)
		hooks[index].Inputs = expandStringMap(hooks[index].Inputs, context)
		hooks[index].Outputs = expandStringMap(hooks[index].Outputs, context)
		hooks[index].Cache.Values = expandStringMap(hooks[index].Cache.Values, context)
	}
}

func expandStrings(values []string, context *StageContext) []string {
	for index := range values {
		values[index] = expand(values[index], context)
	}
	return values
}

func expandStringMap(values map[string]string, context *StageContext) map[string]string {
	for name, value := range values {
		values[name] = expand(value, context)
	}
	return values
}
