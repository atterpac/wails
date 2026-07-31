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
	Packages   []string `name:"format" description:"Distribution package format (specify multiple times)"`
	Pipeline   bool     `name:"pipeline" description:"Build with the typed Wails pipeline instead of compatibility Taskfiles"`
	Parallel   bool     `name:"parallel" description:"Execute independent typed pipeline stages concurrently"`
	From       string   `name:"from" description:"Start typed pipeline execution at this stage"`
	Until      string   `name:"until" description:"Stop typed pipeline execution after this stage"`
	NoCache    bool     `name:"no-cache" description:"Disable typed pipeline artifact cache reads and writes"`
	Resume     bool     `name:"resume" description:"Resume verified completed stages from the latest execution report"`
	CacheDir   string   `name:"cache-dir" description:"Typed pipeline cache directory"`
	Report     string   `name:"report" description:"Typed pipeline execution report path"`
}

type Dev struct {
	Common
}

type Package struct {
	Common
	Plan     bool     `name:"plan" description:"Print the resolved package pipeline without executing it"`
	JSON     bool     `name:"json" description:"Print the package plan as JSON (requires --plan)"`
	Config   string   `name:"config" description:"Path to the build configuration" default:"build/config.yml"`
	Targets  []string `name:"target" description:"Package target as platform/architecture (specify multiple times)"`
	Formats  []string `name:"format" description:"Package format (specify multiple times)"`
	Pipeline bool     `name:"pipeline" description:"Package with the typed Wails pipeline"`
	Parallel bool     `name:"parallel" description:"Execute independent typed pipeline stages concurrently"`
	NoCache  bool     `name:"no-cache" description:"Disable typed pipeline artifact cache reads and writes"`
	Resume   bool     `name:"resume" description:"Resume verified completed stages from the latest execution report"`
	CacheDir string   `name:"cache-dir" description:"Typed pipeline cache directory"`
	Report   string   `name:"report" description:"Typed pipeline execution report path"`
}

type SignWrapper struct {
	Common
	Plan            bool     `name:"plan" description:"Print the resolved signing pipeline without executing it"`
	JSON            bool     `name:"json" description:"Print the signing plan as JSON (requires --plan)"`
	Config          string   `name:"config" description:"Path to the build configuration" default:"build/config.yml"`
	Targets         []string `name:"target" description:"Signing target as platform/architecture (specify multiple times)"`
	Formats         []string `name:"format" description:"Package format to create and sign (specify multiple times)"`
	Pipeline        bool     `name:"pipeline" description:"Sign with the typed Wails pipeline"`
	Parallel        bool     `name:"parallel" description:"Execute independent typed pipeline stages concurrently"`
	NoCache         bool     `name:"no-cache" description:"Disable typed pipeline artifact cache reads and writes"`
	Resume          bool     `name:"resume" description:"Resume verified completed stages from the latest execution report"`
	CacheDir        string   `name:"cache-dir" description:"Typed pipeline cache directory"`
	Report          string   `name:"report" description:"Typed pipeline execution report path"`
	Certificate     string   `name:"certificate" description:"Path to a Windows signing certificate"`
	Thumbprint      string   `name:"thumbprint" description:"Windows certificate thumbprint"`
	Timestamp       string   `name:"timestamp" description:"Timestamp server URL"`
	Identity        string   `name:"identity" description:"macOS signing identity"`
	Entitlements    string   `name:"entitlements" description:"Path to macOS entitlements"`
	HardenedRuntime bool     `name:"hardened-runtime" description:"Enable the macOS hardened runtime"`
	Notarize        bool     `name:"notarize" description:"Notarize and staple macOS application bundles"`
	KeychainProfile string   `name:"keychain-profile" description:"Apple notarytool keychain profile"`
	PGPKey          string   `name:"pgp-key" description:"Path to a Linux PGP private key"`
	Role            string   `name:"role" description:"DEB signing role"`
}
