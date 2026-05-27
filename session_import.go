package codexacp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
)

const (
	codexSessionImportMethod       = "_codex/session/import"
	codexSessionImportChunkMethod  = "_codex/session/importChunk"
	codexSessionCommitImportMethod = "_codex/session/commitImport"
	codexSessionAbortImportMethod  = "_codex/session/abortImport"

	codexSessionImportFormat = codexSessionImportFormatJSON

	maxSessionImportEntries      = 100000
	maxSessionImportChunkEntries = 10000
	maxSessionImportLineBytes    = 10 * 1024 * 1024
	maxSessionImportBytes        = 100 * 1024 * 1024
	sessionImportTTL             = 30 * time.Minute

	jsonFieldImportID  = "importId"
	jsonFieldSessionID = "sessionId"
	jsonFieldCwd       = "cwd"
	jsonFieldFormat    = "format"
	jsonFieldOffset    = "offset"
	jsonFieldEntries   = "entries"
	jsonFieldIndex     = "index"
	jsonFieldSHA256    = "sha256"
	jsonFieldSubpath   = "subpath"

	validationRequired = "required"
)

type sessionImport struct {
	ImportID   string
	SessionID  string
	Cwd        string
	ProjectKey string

	entries map[SessionKey][]SessionStoreEntry
	order   []SessionKey
	count   int
	bytes   int

	UpdatedAt time.Time
}

var sessionImportNow = time.Now

type codexSessionImportParams struct {
	ImportID  string            `json:"importId,omitempty"`
	SessionID string            `json:"sessionId"`
	Cwd       string            `json:"cwd"`
	Format    string            `json:"format,omitempty"`
	Subpath   string            `json:"subpath,omitempty"`
	Offset    int               `json:"offset,omitempty"`
	Entries   []json.RawMessage `json:"entries,omitempty"`
	Lines     []string          `json:"lines,omitempty"`
	JSONL     string            `json:"jsonl,omitempty"`
}

type codexSessionCommitImportParams struct {
	ImportID string `json:"importId"`
	SHA256   string `json:"sha256,omitempty"`
}

type codexSessionAbortImportParams struct {
	ImportID string `json:"importId"`
}

func (a *Agent) importCodexSession(ctx context.Context, params json.RawMessage) (any, error) {
	chunk, err := a.importCodexSessionChunk(ctx, params)
	if err != nil {
		return nil, err
	}

	importID, _ := chunk[jsonFieldImportID].(string)
	commitParams := json.RawMessage(`{"importId":` + strconv.Quote(importID) + `}`)

	return a.commitCodexSessionImport(ctx, commitParams)
}

func (a *Agent) importCodexSessionChunk(ctx context.Context, params json.RawMessage) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var req codexSessionImportParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	if err := req.normalizeEntries(); err != nil {
		return nil, err
	}
	if err := validateSessionImportRequest(req); err != nil {
		return nil, err
	}

	projectKey, _ := projectKeyForDirectory(req.Cwd)
	clean, bytesAccepted, err := validateSessionImportEntries(req.Entries)
	if err != nil {
		return nil, err
	}

	importID := strings.TrimSpace(req.ImportID)
	if importID == "" {
		generated, err := newSessionID()
		if err != nil {
			return nil, fmt.Errorf("generate import id: %w", err)
		}
		importID = generated
	}

	now := sessionImportNow()
	key := SessionKey{ProjectKey: projectKey, SessionID: req.SessionID, Subpath: req.Subpath}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.reapStaleSessionImportsLocked(now)

	imp := a.imports[importID]
	if imp == nil {
		if req.Offset != 0 {
			return nil, acp.NewInvalidParams(map[string]any{jsonFieldOffset: "must be 0 for a new import"})
		}
		imp = &sessionImport{
			ImportID:   importID,
			SessionID:  req.SessionID,
			Cwd:        req.Cwd,
			ProjectKey: projectKey,
			entries:    make(map[SessionKey][]SessionStoreEntry),
			UpdatedAt:  now,
		}
		a.imports[importID] = imp
	} else if err := imp.validateChunk(req, projectKey); err != nil {
		return nil, err
	}

	if req.Offset != len(imp.entries[key]) {
		return nil, acp.NewInvalidParams(map[string]any{
			jsonFieldOffset: map[string]any{
				"expected": len(imp.entries[key]),
				"got":      req.Offset,
			},
		})
	}
	if imp.count+len(clean) > maxSessionImportEntries {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldEntries: "import entry limit exceeded"})
	}
	if imp.bytes+bytesAccepted > maxSessionImportBytes {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldEntries: "import byte limit exceeded"})
	}
	if _, ok := imp.entries[key]; !ok {
		imp.order = append(imp.order, key)
	}

	imp.entries[key] = append(imp.entries[key], clean...)
	imp.count += len(clean)
	imp.bytes += bytesAccepted
	imp.UpdatedAt = now

	return map[string]any{
		jsonFieldImportID:  importID,
		jsonFieldSessionID: imp.SessionID,
		jsonFieldCwd:       imp.Cwd,
		jsonFieldFormat:    codexSessionImportFormat,
		jsonFieldOffset:    len(imp.entries[key]),
		jsonFieldEntries:   imp.count,
		"bytes":            imp.bytes,
	}, nil
}

func (a *Agent) commitCodexSessionImport(ctx context.Context, params json.RawMessage) (any, error) {
	var req codexSessionCommitImportParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	importID := strings.TrimSpace(req.ImportID)
	if importID == "" {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldImportID: validationRequired})
	}

	now := sessionImportNow()

	a.mu.Lock()
	a.reapStaleSessionImportsLocked(now)
	imp := a.imports[importID]
	if imp != nil {
		delete(a.imports, importID)
	}
	a.mu.Unlock()

	if imp == nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldImportID: "unknown import"})
	}

	sum := imp.sha256()
	if req.SHA256 != "" && !strings.EqualFold(req.SHA256, sum) {
		return nil, acp.NewInvalidParams(map[string]any{
			jsonFieldSHA256: map[string]any{"expected": req.SHA256, "actual": sum},
		})
	}
	if err := a.replaceStoreImport(ctx, a.sessionStore(), imp); err != nil {
		return nil, err
	}

	return map[string]any{
		jsonFieldImportID:  imp.ImportID,
		jsonFieldSessionID: imp.SessionID,
		jsonFieldCwd:       imp.Cwd,
		jsonFieldFormat:    codexSessionImportFormat,
		jsonFieldEntries:   imp.count,
		"bytes":            imp.bytes,
		jsonFieldSHA256:    sum,
	}, nil
}

func (a *Agent) abortCodexSessionImport(_ context.Context, params json.RawMessage) (any, error) {
	var req codexSessionAbortImportParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	importID := strings.TrimSpace(req.ImportID)
	if importID == "" {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldImportID: validationRequired})
	}

	now := sessionImportNow()

	a.mu.Lock()
	a.reapStaleSessionImportsLocked(now)
	_, existed := a.imports[importID]
	delete(a.imports, importID)
	a.mu.Unlock()

	return map[string]any{"aborted": existed}, nil
}

func (a *Agent) reapStaleSessionImportsLocked(now time.Time) {
	for importID, imp := range a.imports {
		if imp == nil || (!imp.UpdatedAt.IsZero() && now.Sub(imp.UpdatedAt) > sessionImportTTL) {
			delete(a.imports, importID)
		}
	}
}

func (req *codexSessionImportParams) normalizeEntries() error {
	if req.JSONL != "" {
		entries, err := jsonlLinesToRaw(req.JSONL)
		if err != nil {
			return err
		}
		req.Entries = append(req.Entries, entries...)
	}
	for _, line := range req.Lines {
		req.Entries = append(req.Entries, json.RawMessage(line))
	}

	return nil
}

func jsonlLinesToRaw(text string) ([]json.RawMessage, error) {
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(nil, maxSessionImportLineBytes)

	var entries []json.RawMessage
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		entries = append(entries, json.RawMessage(line))
	}
	if err := scanner.Err(); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldEntries: err.Error()})
	}

	return entries, nil
}

func validateSessionImportRequest(req codexSessionImportParams) error {
	if req.SessionID == "" {
		return acp.NewInvalidParams(map[string]any{jsonFieldSessionID: validationRequired})
	}
	if err := validateRequiredAbsolutePath(jsonFieldCwd, req.Cwd); err != nil {
		return err
	}
	if req.Format != "" && req.Format != codexSessionImportFormat {
		return acp.NewInvalidParams(map[string]any{jsonFieldFormat: fmt.Sprintf("must be %q", codexSessionImportFormat)})
	}
	if req.Subpath != "" && !isSafeSessionSubpath(req.Subpath) {
		return acp.NewInvalidParams(map[string]any{jsonFieldSubpath: "must be a relative session subpath"})
	}
	if req.Offset < 0 {
		return acp.NewInvalidParams(map[string]any{jsonFieldOffset: "must be non-negative"})
	}
	if len(req.Entries) == 0 {
		return acp.NewInvalidParams(map[string]any{jsonFieldEntries: validationRequired})
	}
	if len(req.Entries) > maxSessionImportChunkEntries {
		return acp.NewInvalidParams(map[string]any{jsonFieldEntries: "chunk entry limit exceeded"})
	}

	return nil
}

func validateSessionImportEntries(entries []json.RawMessage) ([]SessionStoreEntry, int, error) {
	clean := make([]SessionStoreEntry, 0, len(entries))
	total := 0

	for i, entry := range entries {
		trimmed := bytes.TrimSpace(entry)
		if len(trimmed) == 0 {
			return nil, 0, acp.NewInvalidParams(map[string]any{jsonFieldEntries: map[string]any{jsonFieldIndex: i, jsonFieldError: validationRequired}})
		}
		if len(trimmed) > maxSessionImportLineBytes {
			return nil, 0, acp.NewInvalidParams(map[string]any{jsonFieldEntries: map[string]any{jsonFieldIndex: i, jsonFieldError: "line byte limit exceeded"}})
		}

		var obj map[string]json.RawMessage
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		if err := dec.Decode(&obj); err != nil {
			return nil, 0, acp.NewInvalidParams(map[string]any{jsonFieldEntries: map[string]any{jsonFieldIndex: i, jsonFieldError: err.Error()}})
		}
		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			return nil, 0, acp.NewInvalidParams(map[string]any{jsonFieldEntries: map[string]any{jsonFieldIndex: i, jsonFieldError: "must contain one JSON object"}})
		}
		if obj == nil {
			return nil, 0, acp.NewInvalidParams(map[string]any{jsonFieldEntries: map[string]any{jsonFieldIndex: i, jsonFieldError: "must be a JSON object"}})
		}

		clean = append(clean, append(SessionStoreEntry(nil), trimmed...))
		total += len(trimmed) + 1
	}

	return clean, total, nil
}

func (imp *sessionImport) validateChunk(req codexSessionImportParams, projectKey string) error {
	if imp.SessionID != req.SessionID {
		return acp.NewInvalidParams(map[string]any{jsonFieldSessionID: "does not match existing import"})
	}
	if imp.Cwd != req.Cwd || imp.ProjectKey != projectKey {
		return acp.NewInvalidParams(map[string]any{jsonFieldCwd: "does not match existing import"})
	}

	return nil
}

func (imp *sessionImport) sha256() string {
	hash := sha256.New()
	for _, key := range imp.order {
		for _, entry := range imp.entries[key] {
			_, _ = hash.Write(bytes.TrimSpace(entry))
			_, _ = hash.Write([]byte{'\n'})
		}
	}

	return hex.EncodeToString(hash.Sum(nil))
}

func (a *Agent) replaceStoreImport(ctx context.Context, store SessionStore, imp *sessionImport) error {
	mainKey := SessionKey{ProjectKey: imp.ProjectKey, SessionID: imp.SessionID}
	existing, err := store.Load(ctx, mainKey)
	if err != nil {
		return fmt.Errorf("load existing imported session: %w", err)
	}
	if len(existing) > 0 {
		replacer, ok := store.(SessionStoreReplacer)
		if !ok {
			return acp.NewInvalidParams(map[string]any{jsonFieldSessionID: "session already exists and store does not support atomic replacement"})
		}
		if err := replacer.ReplaceSession(ctx, mainKey, sessionImportReplacements(imp)); err != nil {
			return fmt.Errorf("replace existing imported session: %w", err)
		}
		return nil
	}

	for _, key := range imp.order {
		if err := store.Append(ctx, key, imp.entries[key]); err != nil {
			return fmt.Errorf("append imported session: %w", err)
		}
	}

	return nil
}

func sessionImportReplacements(imp *sessionImport) []SessionStoreReplacement {
	replacements := make([]SessionStoreReplacement, 0, len(imp.order))
	for _, key := range imp.order {
		replacements = append(replacements, SessionStoreReplacement{
			Key:     key,
			Entries: cloneStoreEntries(imp.entries[key]),
		})
	}

	return replacements
}

func (a *Agent) sessionStore() SessionStore {
	if a.options.SessionStore != nil {
		return a.options.SessionStore
	}
	return a.importStore
}

func validateRequiredAbsolutePath(field string, value string) error {
	if strings.TrimSpace(value) == "" {
		return acp.NewInvalidParams(map[string]any{field: validationRequired})
	}
	if !filepath.IsAbs(value) {
		return acp.NewInvalidParams(map[string]any{field: "must be absolute"})
	}

	return nil
}

func isSafeSessionSubpath(subpath string) bool {
	if subpath == "" ||
		filepath.IsAbs(subpath) ||
		strings.HasPrefix(subpath, "/") ||
		strings.HasPrefix(subpath, "\\") ||
		strings.Contains(subpath, "\x00") ||
		filepath.VolumeName(subpath) != "" {
		return false
	}

	for _, part := range strings.FieldsFunc(subpath, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == "" || part == "." || part == ".." || strings.Contains(part, ":") {
			return false
		}
	}

	return true
}
