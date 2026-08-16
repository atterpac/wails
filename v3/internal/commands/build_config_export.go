package commands

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v3/internal/term"
)

const defaultBuildConfigReference = "build/config.reference.yml"

//go:embed build_config_reference.yml
var buildConfigReference []byte

func exportBuildConfigReference(path string) error {
	if path == "" {
		path = defaultBuildConfigReference
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("build configuration reference already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check build configuration reference %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create build configuration reference directory: %w", err)
	}
	if err := os.WriteFile(path, buildConfigReference, 0o644); err != nil {
		return fmt.Errorf("write build configuration reference: %w", err)
	}
	term.Success("Wrote " + path)
	return nil
}
