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
	// seedManifestName is the seed-owned ownership manifest written into the
	// seed root. It records the relative paths the seed feature manages so
	// re-seeding never clobbers an operator-authored file.
	seedManifestName = ".seed-manifest.json"
	// seedBackupSuffix names the sidecar that holds the prior bytes of a managed
	// seed file whenever its content changes.
	seedBackupSuffix = ".seed.bak"
	// codexHomeConfigFileName is the only file inside CODEX_HOME the Codex CLI
	// loads configuration from; everything else the app-server keeps there is
	// state, and the managed-config overlays live outside the home entirely
	// (`/etc/codex/...`). Measured against codex-cli 0.147.0, whose `doctor`
	// reports exactly this path as the loaded config and ignores sibling
	// `config.json`, `managed_config.toml`, and `requirements.toml` files
	// planted in the same home.
	codexHomeConfigFileName = "config.toml"
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
// verbatim (callers reference secrets from WithEnv via Codex env_key), except
// that the seeded CODEX_HOME config file is refused when it declares a config
// root reserved to the adapter — see seedConfigDefinesReservedRoot.
//
// Writes are routed through an ownership manifest (.seed-manifest.json in
// the seed root) so seeding never overwrites a file the seed feature did not
// create: a first write records the path in the manifest; a managed file is
// overwritten (keeping a .seed.bak of the prior bytes when the content
// changes); an existing unmanaged file fails closed, leaving every file
// untouched.
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

		if isCodexHomeConfigPath(home, path) && seedConfigDefinesReservedRoot(files[name]) {
			return unsupportedField(fmt.Sprintf("seedFiles[%q]", name))
		}

		targets = append(targets, seedTarget{name: name, rel: rel, path: path})
	}

	manifest, err := loadSeedManifest(home)
	if err != nil {
		return err
	}

	// Fail closed if any target already exists but the seed feature does not own
	// it, before writing anything.
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
// backed up to a .seed.bak sidecar before the new bytes are written.
func writeSeedTarget(target seedTarget, data []byte) error {
	if !target.exists {
		return writeSeedBytes(target.path, data)
	}

	current, err := os.ReadFile(target.path) // #nosec G304 -- seed-owned managed seed target under confined CODEX_HOME.
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

	// #nosec G703 G304 -- path is confined under CODEX_HOME (resolveSeedPath rejects abs/.. escapes) or is a seed-owned manifest/backup sidecar.
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

// manifestOwns reports whether rel is a seed-managed relative path.
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
	data, err := os.ReadFile(filepath.Join(home, seedManifestName)) // #nosec G304 -- seed-owned manifest under confined CODEX_HOME.
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

// isCodexHomeConfigPath reports whether an already-confined seed destination is
// the CODEX_HOME config file. The comparison is on the joined path rather than
// the caller's key so spellings that normalise to the same file — `config.toml`
// and `./config.toml` — cannot differ. The base name is compared case
// insensitively because on a case-folding filesystem `Config.toml` is that same
// file, and on a case-sensitive one the cost of the extra match is a seed file
// Codex would not have read anyway.
func isCodexHomeConfigPath(home string, path string) bool {
	return filepath.Dir(path) == filepath.Clean(home) &&
		strings.EqualFold(filepath.Base(path), codexHomeConfigFileName)
}

// seedConfigDefinesReservedRoot reports whether a seeded CODEX_HOME config file
// declares one of codexReservedConfigRoots. Seeding and `-c key=value` land in
// the same app-server keyspace, so a root reserved on the override surface has
// to be reserved here too; otherwise the reservation is a property of one entry
// point instead of the keyspace, and an `inherit`/`exclude` seeded on this path
// survives into every thread, because per-thread config only ever authors
// `shell_environment_policy.set`.
//
// The module carries no TOML parser, so this reads the line structure TOML
// guarantees instead of parsing values: a table header is the first
// non-whitespace on its line, and a key, its `=`, and the start of its value
// share one line. That is enough to see every root a document can declare.
//
// The scan deliberately tracks neither string nor table context. A context
// tracker that can be fooled is somewhere to hide a root — a decoy `[table]`
// inside a multi-line string would otherwise re-parent every key after it — so
// a reserved root is rejected wherever it could be a key, including inside a
// string value and beneath a parent table. Comment lines need no special case:
// `#` is not a bare-key character, so a commented root normalises to a root
// name no reservation claims.
func seedConfigDefinesReservedRoot(content string) bool {
	for line := range strings.Lines(strings.TrimPrefix(content, "\ufeff")) {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "[") && tableHeaderDeclaresReservedRoot(trimmed) {
			return true
		}

		if key, _, found := strings.Cut(trimmed, "="); found && codexConfigRootIsReserved(key) {
			return true
		}
	}

	return false
}

// tableHeaderDeclaresReservedRoot reports whether a `[table]` or `[[array]]`
// header names a reserved root. Where the header body ends is ambiguous without
// a parser — a quoted key may contain `]` and a trailing comment may add one, so
// neither the first nor the last bracket is reliably the terminator — so every
// candidate cut is tested, along with the uncut body for a header that never
// closes.
func tableHeaderDeclaresReservedRoot(line string) bool {
	body := strings.TrimLeft(line, "[")
	if codexConfigRootIsReserved(body) {
		return true
	}

	for index, char := range body {
		if char == ']' && codexConfigRootIsReserved(body[:index]) {
			return true
		}
	}

	return false
}

// codexConfigRootIsReserved normalises a TOML key the way the `-c` override
// surface does and reads the single reservation list, so both doors into the
// keyspace can never disagree about what is reserved.
func codexConfigRootIsReserved(key string) bool {
	_, reserved := codexReservedConfigRoots[codexConfigRootKey(key)]

	return reserved
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
