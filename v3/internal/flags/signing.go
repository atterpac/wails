package flags

type SigningSetup struct {
	Platforms []string `name:"platform" description:"Platform(s) to configure (darwin, windows, linux). If not specified, auto-detects from build directory."`
	Config    string   `name:"config" description:"Path to the typed build configuration" default:"build/config.yml"`
}

type EntitlementsSetup struct {
	Output string `name:"output" description:"Output path for entitlements.plist (default: build/darwin/entitlements.plist)"`
}
