package flags

type Build struct {
	Common
	Tags       string   `name:"tags" description:"Additional build tags to pass to the Go compiler (comma-separated)"`
	Obfuscated bool     `name:"obfuscated" description:"Build with garble and stable obfuscated binding IDs"`
	GarbleArgs string   `name:"garbleargs" description:"Additional arguments to pass to garble before the build command"`
	Plan       bool     `name:"plan" description:"Print the resolved Wails build plan without executing it"`
	JSON       bool     `name:"json" description:"Print the build plan as JSON (requires --plan)"`
	Config     string   `name:"config" description:"Path to the build configuration" default:"build/config.yml"`
	Targets    []string `name:"target" description:"Build target as platform/architecture (specify multiple times)"`
	Pipeline   bool     `name:"pipeline" description:"Build with the typed Wails pipeline instead of compatibility Taskfiles"`
	Parallel   bool     `name:"parallel" description:"Execute independent typed pipeline stages concurrently"`
	From       string   `name:"from" description:"Start typed pipeline execution at this stage"`
	Until      string   `name:"until" description:"Stop typed pipeline execution after this stage"`
}

type Dev struct {
	Common
}

type Package struct {
	Common
}

type SignWrapper struct {
	Common
}
