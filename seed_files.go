package codexacp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// seedManifestName is the wagie-owned ownership manifest written into the
	// seed root. It records the relative paths wagie manages so re-seeding never
	// clobbers an operator-authored file.
	seedManifestName = ".wagie-seed-manifest.json"
	// seedBackupSuffix names the sidecar that holds the prior bytes of a managed
	// seed file whenever its content changes.
	seedBackupSuffix = ".wagie.bak"
)

// seedTarget is a validated, home-confined destination for one seed file.
type seedTarget struct {
	// name is the caller-supplied relative key (used in error messages).
	name string
	// rel is the canonical slash-separated manifest key for the target.
	rel string
	// path is the absolute on-disk destination under the seed root.
	path string
	// exists is whether path already exists on disk.
	exists bool
}

// writeSeedFiles writes the configured seed files into the resolved CODEX_HOME
// before the Codex CLI launches, so Codex reads them as its own config. The
// home argument is CODEX_HOME verbatim (see WithHome / internal/codex command
// env). Files are written with parent directories 0o700 and files 0o600. Paths
// are relative and confined to home; absolute paths, ".." escapes, and empty
// keys fail closed with the uniform unsupported error. Contents are written
// verbatim (callers reference secrets from WithEnv via Codex env_key).
//
// Writes are routed through an ownership manifest (.wagie-seed-manifest.json in
// the seed root) so seeding never overwrites a file wagie did not create: a
// first write records the path in the manifest; a managed file is overwritten
// (keeping a .wagie.bak of the prior bytes when the content changes); an
// existing unmanaged file fails closed, leaving every file untouched.
func writeSeedFiles(home string, files map[string]string) error {
	if len(files) == 0 {
		return nil
	}

	if strings.TrimSpace(home) == "" {
		return unsupportedField("seedFiles")
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}

	sort.Strings(names)

	// Resolve and confine every path before touching disk so a rejected path
	// leaves all files untouched.
	targets := make([]seedTarget, 0, len(names))
	for _, name := range names {
		path, rel, err := resolveSeedPath(home, name)
		if err != nil {
			return err
		}

		targets = append(targets, seedTarget{name: name, rel: rel, path: path})
	}

	manifest, err := loadSeedManifest(home)
	if err != nil {
		return err
	}

	// Fail closed if any target already exists but wagie does not own it, before
	// writing anything.
	for i := range targets {
		exists, existsErr := seedTargetExists(targets[i].path)
		if existsErr != nil {
			return existsErr
		}

		if exists && !manifestOwns(manifest, targets[i].rel) {
			return unsupportedField(fmt.Sprintf("seedFiles[%q]", targets[i].name))
		}

		targets[i].exists = exists
	}

	added := false

	for i := range targets {
		if err := writeSeedTarget(targets[i], []byte(files[targets[i].name])); err != nil {
			return err
		}

		if !targets[i].exists {
			manifest = append(manifest, targets[i].rel)
			added = true
		}
	}

	if added {
		return saveSeedManifest(home, manifest)
	}

	return nil
}

// writeSeedTarget writes data to the target. A new target is written directly; a
// managed target is compared to its on-disk bytes and, only when they differ, is
// backed up to a .wagie.bak sidecar before the new bytes are written.
func writeSeedTarget(target seedTarget, data []byte) error {
	if !target.exists {
		return writeSeedBytes(target.path, data)
	}

	current, err := os.ReadFile(target.path) // #nosec G304 -- wagie-owned managed seed target under confined CODEX_HOME.
	if err != nil {
		return fmt.Errorf("read managed seed file: %w", err)
	}

	if bytes.Equal(current, data) {
		return nil
	}

	if err := writeSeedBytes(target.path+seedBackupSuffix, current); err != nil {
		return err
	}

	return writeSeedBytes(target.path, data)
}

// writeSeedBytes creates the parent directory (0o700) and writes the file (0o600).
func writeSeedBytes(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create seed file directory: %w", err)
	}

	// #nosec G703 G304 -- path is confined under CODEX_HOME (resolveSeedPath rejects abs/.. escapes) or is a wagie-owned manifest/backup sidecar.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write seed file: %w", err)
	}

	return nil
}

// seedTargetExists reports whether path exists, distinguishing a missing file
// from an unexpected stat error.
func seedTargetExists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("stat seed file: %w", err)
	}

	return true, nil
}

// manifestOwns reports whether rel is a wagie-managed relative path.
func manifestOwns(manifest []string, rel string) bool {
	for _, entry := range manifest {
		if entry == rel {
			return true
		}
	}

	return false
}

// loadSeedManifest reads the ownership manifest fresh from the seed root,
// returning an empty manifest when it is absent.
func loadSeedManifest(home string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(home, seedManifestName)) // #nosec G304 -- wagie-owned manifest under confined CODEX_HOME.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("read seed manifest: %w", err)
	}

	var entries []string
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse seed manifest: %w", err)
	}

	return entries, nil
}

// saveSeedManifest writes the ownership manifest as a sorted, deduplicated JSON
// array of managed relative paths.
func saveSeedManifest(home string, manifest []string) error {
	seen := make(map[string]struct{}, len(manifest))
	entries := make([]string, 0, len(manifest))

	for _, entry := range manifest {
		if _, ok := seen[entry]; ok {
			continue
		}

		seen[entry] = struct{}{}

		entries = append(entries, entry)
	}

	sort.Strings(entries)

	// A []string always marshals; the error is structurally unreachable.
	payload, _ := json.Marshal(entries)

	return writeSeedBytes(filepath.Join(home, seedManifestName), payload)
}

// resolveSeedPath validates a relative seed path and joins it under home. It
// rejects empty keys, absolute paths, and any ".." segment, returning the
// absolute destination and the canonical slash-separated manifest key.
func resolveSeedPath(home string, name string) (string, string, error) {
	field := fmt.Sprintf("seedFiles[%q]", name)

	if strings.TrimSpace(name) == "" {
		return "", "", unsupportedField(field)
	}

	if filepath.IsAbs(name) {
		return "", "", unsupportedField(field)
	}

	slashed := filepath.ToSlash(name)
	for _, segment := range strings.Split(slashed, "/") {
		if segment == ".." {
			return "", "", unsupportedField(field)
		}
	}

	return filepath.Join(home, filepath.FromSlash(slashed)), slashed, nil
}
