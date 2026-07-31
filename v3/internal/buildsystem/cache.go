package buildsystem

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const cacheFormatVersion = 1

type cacheStore struct {
	root          string
	projectRoot   string
	workspaceHash string
}

type cacheManifest struct {
	Version     int              `json:"version"`
	Stage       string           `json:"stage"`
	Fingerprint string           `json:"fingerprint"`
	Outputs     []cachedArtifact `json:"outputs"`
}

type cachedArtifact struct {
	Artifact Artifact `json:"artifact"`
	Stored   string   `json:"stored"`
	SHA256   string   `json:"sha256"`
}

func newCacheStore(plan *Plan, directory, reportPath string) (*cacheStore, error) {
	if directory == "" {
		directory = filepath.Join(plan.Project.Root, ".wails", "build", "cache")
	}
	hash, err := hashWorkspace(plan, directory, reportPath)
	if err != nil {
		return nil, fmt.Errorf("fingerprint project sources: %w", err)
	}
	return &cacheStore{root: directory, projectRoot: plan.Project.Root, workspaceHash: hash}, nil
}

func (store *cacheStore) fingerprint(plan *Plan, stage Stage, artifacts map[string]Artifact) (string, error) {
	stageContext := &StageContext{Plan: plan, Stage: stage, Artifacts: artifacts}
	inputs := make(map[string]string, len(stage.Inputs))
	for _, input := range stage.Inputs {
		if input.Path == "" {
			inputs[input.Reference()] = "in-memory"
			continue
		}
		hash, err := hashFilesystemPath(resolvePath(plan.Project.Root, input.Path))
		if err != nil {
			return "", fmt.Errorf("fingerprint input %q: %w", input.Reference(), err)
		}
		inputs[input.Reference()] = hash
	}
	hookInputs := make(map[string]string)
	for _, hook := range append(append([]Hook(nil), stage.Before...), stage.After...) {
		if hook.Status == "skipped" {
			continue
		}
		for _, source := range hook.Cache.Files {
			path := expand(source, stageContext)
			if !filepath.IsAbs(path) {
				path = resolvePath(plan.Project.Root, path)
			}
			hash, err := hashFilesystemPath(path)
			if err != nil {
				return "", fmt.Errorf("fingerprint hook cache file %s: %w", path, err)
			}
			hookInputs[hook.ID+"/file/"+source] = hash
		}
		for _, name := range hook.Cache.Environment {
			hookInputs[hook.ID+"/env/"+name] = os.Getenv(name)
		}
		for name, value := range hook.Cache.Values {
			hookInputs[hook.ID+"/value/"+name] = expand(value, stageContext)
		}
	}
	payload := struct {
		Version       int               `json:"version"`
		PlanVersion   string            `json:"planVersion"`
		Mode          string            `json:"mode"`
		Goal          string            `json:"goal"`
		Project       Project           `json:"project"`
		Signing       SigningConfig     `json:"signing"`
		Stage         Stage             `json:"stage"`
		WorkspaceHash string            `json:"workspaceHash"`
		Inputs        map[string]string `json:"inputs"`
		HookInputs    map[string]string `json:"hookInputs"`
	}{cacheFormatVersion, plan.Version, plan.Mode, plan.Goal, plan.Project, plan.Signing, stage, store.workspaceHash, inputs, hookInputs}
	payload.Project.Root = ""
	payload.Project.Config = ""
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode stage fingerprint: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (store *cacheStore) restore(stage Stage, fingerprint string) (bool, error) {
	directory := filepath.Join(store.root, fingerprint)
	data, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var manifest cacheManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false, fmt.Errorf("decode cache manifest: %w", err)
	}
	if manifest.Version != cacheFormatVersion || manifest.Stage != stage.Reference() || manifest.Fingerprint != fingerprint {
		return false, fmt.Errorf("cache manifest does not match stage %s", stage.Reference())
	}
	for _, output := range manifest.Outputs {
		if output.Artifact.Path == "" {
			continue
		}
		source := filepath.Join(directory, filepath.FromSlash(output.Stored))
		hash, err := hashFilesystemPath(source)
		if err != nil {
			return false, fmt.Errorf("verify cached artifact %q: %w", output.Artifact.Reference(), err)
		}
		if hash != output.SHA256 {
			return false, fmt.Errorf("cached artifact %q failed integrity verification", output.Artifact.Reference())
		}
		destination := resolvePath(store.projectRoot, output.Artifact.Path)
		if err := replacePath(source, destination); err != nil {
			return false, fmt.Errorf("restore artifact %q: %w", output.Artifact.Reference(), err)
		}
	}
	return true, nil
}

func (store *cacheStore) store(stage Stage, fingerprint string) error {
	if err := os.MkdirAll(store.root, 0o755); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(store.root, ".pending-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	manifest := cacheManifest{Version: cacheFormatVersion, Stage: stage.Reference(), Fingerprint: fingerprint}
	for index, output := range stage.Outputs {
		if output.Path == "" {
			continue
		}
		source := resolvePath(store.projectRoot, output.Path)
		if _, err := os.Lstat(source); os.IsNotExist(err) && output.Optional {
			continue
		} else if err != nil {
			return err
		}
		stored := filepath.ToSlash(filepath.Join("outputs", fmt.Sprintf("%d", index)))
		hash, err := hashFilesystemPath(source)
		if err != nil {
			return err
		}
		if err := copyPath(source, filepath.Join(temporary, filepath.FromSlash(stored))); err != nil {
			return err
		}
		manifest.Outputs = append(manifest.Outputs, cachedArtifact{Artifact: output, Stored: stored, SHA256: hash})
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(temporary, "manifest.json"), data, 0o644); err != nil {
		return err
	}
	destination := filepath.Join(store.root, fingerprint)
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	return os.Rename(temporary, destination)
}

func hashWorkspace(plan *Plan, runtimeExclusions ...string) (string, error) {
	exclusions := []string{
		filepath.Join(plan.Project.Root, ".git"),
		filepath.Join(plan.Project.Root, ".wails"),
		filepath.Join(plan.Project.Root, ".beads"),
		filepath.Join(plan.Project.Root, "node_modules"),
	}
	for _, exclusion := range runtimeExclusions {
		if exclusion != "" {
			exclusions = append(exclusions, exclusion)
		}
	}
	if plan.Project.Output != "" {
		exclusions = append(exclusions, resolvePath(plan.Project.Root, plan.Project.Output))
	}
	for _, stage := range plan.Stages {
		for _, output := range stage.Outputs {
			if output.Path != "" {
				exclusions = append(exclusions, resolvePath(plan.Project.Root, output.Path))
			}
		}
	}
	hasher := sha256.New()
	err := filepath.WalkDir(plan.Project.Root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != plan.Project.Root && excludedPath(path, exclusions) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != plan.Project.Root && entry.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(plan.Project.Root, path)
		if err != nil {
			return err
		}
		_, _ = io.WriteString(hasher, filepath.ToSlash(relative)+"\x00")
		return hashEntry(hasher, path, entry)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func excludedPath(path string, exclusions []string) bool {
	path = filepath.Clean(path)
	for _, exclusion := range exclusions {
		exclusion = filepath.Clean(exclusion)
		if path == exclusion || strings.HasPrefix(path, exclusion+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func hashFilesystemPath(path string) (string, error) {
	hasher := sha256.New()
	if _, err := os.Lstat(path); err != nil {
		return "", err
	}
	base := path
	err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(base, current)
		if err != nil {
			return err
		}
		_, _ = io.WriteString(hasher, filepath.ToSlash(relative)+"\x00")
		return hashEntry(hasher, current, entry)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func hashEntry(writer io.Writer, path string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	_, _ = io.WriteString(writer, info.Mode().String()+"\x00")
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		_, _ = io.WriteString(writer, target+"\x00")
		return nil
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(writer, file)
	return err
}

func replacePath(source, destination string) error {
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	return copyPath(source, destination)
}

func copyPath(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		return os.Symlink(target, destination)
	}
	if !info.IsDir() {
		return copyFile(source, destination)
	}
	if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chmod(destination, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := copyPath(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
