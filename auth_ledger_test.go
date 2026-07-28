package codexacp

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/savid/acp-go-codex/internal/codex"
)

type fakeLedgerFile struct {
	name     string
	writeErr error
	chmodErr error
	syncErr  error
	closeErr error
	closed   int
}

func (f *fakeLedgerFile) Name() string              { return f.name }
func (f *fakeLedgerFile) Write([]byte) (int, error) { return 0, f.writeErr }
func (f *fakeLedgerFile) Chmod(os.FileMode) error   { return f.chmodErr }
func (f *fakeLedgerFile) Sync() error               { return f.syncErr }
func (f *fakeLedgerFile) Close() error {
	f.closed++

	return f.closeErr
}

func newTestLedger(t *testing.T) *authLedger {
	t.Helper()

	ledger, err := newAuthLedger(Options{ProviderAuthRoot: t.TempDir(), Home: t.TempDir()})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}

	return ledger
}

func sampleLedgerRecord() authLedgerRecord {
	return authLedgerRecord{
		ProviderID:         authProviderOpenAI,
		ConnectionID:       "connection-1",
		Revision:           2,
		BindingGeneration:  3,
		FlowID:             "flow-1",
		AuthorizeRequestID: "request-1",
		State:              authLedgerConfirmed,
		CreatedAt:          10,
		UpdatedAt:          20,
	}
}

func TestValidateProviderAuthOptions(t *testing.T) {
	if err := validateProviderAuthOptions(Options{ProviderAuthRoot: "/abs", ProviderAuthDirectHome: "/abs"}); err != nil {
		t.Fatalf("absolute paths were rejected: %v", err)
	}

	if err := validateProviderAuthOptions(Options{}); err != nil {
		t.Fatalf("unset paths were rejected: %v", err)
	}

	err := validateProviderAuthOptions(Options{ProviderAuthRoot: "rel", ProviderAuthDirectHome: "rel"})
	if err == nil {
		t.Fatal("relative paths were accepted")
	}
}

func TestAuthLedgerRootConfigured(t *testing.T) {
	if authLedgerRootConfigured(Options{}) {
		t.Fatal("an unset root reported configured")
	}

	if !authLedgerRootConfigured(Options{ProviderAuthRoot: "/root"}) {
		t.Fatal("a set root reported unconfigured")
	}
}

func TestNewAuthLedgerRejectsARelativeRoot(t *testing.T) {
	if _, err := newAuthLedger(Options{ProviderAuthRoot: "relative"}); err == nil {
		t.Fatal("a relative root was accepted")
	}
}

// TestNewAuthLedgerRestrictsTheConfiguredRoot pins the mode of the directory
// the operator named, not only of the leaf under it: a pre-existing root keeps
// whatever mode it was created with, and the ledger is only as private as the
// directory holding it.
func TestNewAuthLedgerRestrictsTheConfiguredRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("seed root: %v", err)
	}

	if _, err := newAuthLedger(Options{ProviderAuthRoot: root, Home: "/home"}); err != nil {
		t.Fatalf("new ledger: %v", err)
	}

	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}

	if info.Mode().Perm() != authLedgerDirMode {
		t.Fatalf("root mode = %v, want %v", info.Mode().Perm(), os.FileMode(authLedgerDirMode))
	}
}

func TestNewAuthLedgerPreparationFailures(t *testing.T) {
	options := Options{ProviderAuthRoot: t.TempDir(), Home: "/home"}

	cases := map[string]func(){
		"mkdir": func() {
			ledgerMkdirAll = func(string, os.FileMode) error { return errors.New("mkdir") }
		},
		"chmod": func() {
			ledgerChmod = func(string, os.FileMode) error { return errors.New("chmod") }
		},
		// The configured root and the leaf under it are prepared separately, so
		// a failure that only the leaf reaches is its own case.
		"leafmkdir": func() {
			mkdirAll := ledgerMkdirAll
			ledgerMkdirAll = func(path string, mode os.FileMode) error {
				if path == options.ProviderAuthRoot {
					return mkdirAll(path, mode)
				}

				return errors.New("mkdir")
			}
		},
		"leafchmod": func() {
			chmod := ledgerChmod
			ledgerChmod = func(path string, mode os.FileMode) error {
				if path == options.ProviderAuthRoot {
					return chmod(path, mode)
				}

				return errors.New("chmod")
			}
		},
		"stat": func() {
			ledgerStat = func(string) (os.FileInfo, error) { return nil, errors.New("stat") }
		},
		"notdir": func() {
			ledgerStat = func(path string) (os.FileInfo, error) { return os.Stat(mustTempFile(path)) }
		},
		"writable": func() {
			ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errors.New("temp") }
		},
	}

	for name, install := range cases {
		t.Run(name, func(t *testing.T) {
			restoreLedgerHooks(t)
			install()

			if _, err := newAuthLedger(options); err == nil {
				t.Fatal("an unusable root was accepted")
			}
		})
	}
}

func mustTempFile(dir string) string {
	file, err := os.CreateTemp(filepath.Dir(dir), "notdir-")
	if err != nil {
		panic(err)
	}

	name := file.Name()
	_ = file.Close()

	return name
}

func restoreLedgerHooks(t *testing.T) {
	t.Helper()

	mkdirAll, chmod, stat, rename := ledgerMkdirAll, ledgerChmod, ledgerStat, ledgerRename
	open, readFile, readDir, remove := ledgerOpen, ledgerReadFile, ledgerReadDir, ledgerRemove
	createTemp, marshal := ledgerCreateTemp, ledgerMarshal

	t.Cleanup(func() {
		ledgerMkdirAll, ledgerChmod, ledgerStat, ledgerRename = mkdirAll, chmod, stat, rename
		ledgerOpen, ledgerReadFile, ledgerReadDir, ledgerRemove = open, readFile, readDir, remove
		ledgerCreateTemp, ledgerMarshal = createTemp, marshal
	})
}

func TestAuthLedgerHomeKeySeparatesHomes(t *testing.T) {
	if authLedgerHomeKey("/a") == authLedgerHomeKey("/b") {
		t.Fatal("two homes share one ledger key")
	}

	if authLedgerHomeKey("/a/") != authLedgerHomeKey("/a") {
		t.Fatal("the ledger key is not path-cleaned")
	}
}

func TestAuthLedgerRoundTrip(t *testing.T) {
	ledger := newTestLedger(t)

	if _, ok, err := ledger.read(authProviderOpenAI); err != nil || ok {
		t.Fatalf("an absent entry read as ok=%v err=%v", ok, err)
	}

	record := sampleLedgerRecord()
	if err := ledger.write(record); err != nil {
		t.Fatalf("write: %v", err)
	}

	read, ok, err := ledger.read(authProviderOpenAI)
	if err != nil || !ok {
		t.Fatalf("read: ok=%v err=%v", ok, err)
	}

	if read != record {
		t.Fatalf("read %+v, want %+v", read, record)
	}

	records, err := ledger.list()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(records) != 1 || records[0] != record {
		t.Fatalf("list = %+v", records)
	}
}

func TestAuthLedgerEntryIsValuesFree(t *testing.T) {
	ledger := newTestLedger(t)

	if err := ledger.write(sampleLedgerRecord()); err != nil {
		t.Fatalf("write: %v", err)
	}

	contents, err := os.ReadFile(ledger.path(authProviderOpenAI))
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}

	var fields map[string]any
	if decodeErr := json.Unmarshal(contents, &fields); decodeErr != nil {
		t.Fatalf("decode entry: %v", decodeErr)
	}

	allowed := map[string]struct{}{
		"providerId": {}, "connectionId": {}, "revision": {}, "bindingGeneration": {},
		"flowId": {}, "authorizeRequestId": {}, "state": {}, "createdAt": {}, "updatedAt": {},
	}

	for key := range fields {
		if _, ok := allowed[key]; !ok {
			t.Fatalf("ledger entry carries %q", key)
		}
	}

	info, err := os.Stat(ledger.path(authProviderOpenAI))
	if err != nil {
		t.Fatalf("stat entry: %v", err)
	}

	if info.Mode().Perm() != authLedgerFileMode {
		t.Fatalf("entry mode = %v", info.Mode().Perm())
	}
}

func TestAuthLedgerReadFailures(t *testing.T) {
	ledger := newTestLedger(t)

	restoreLedgerHooks(t)

	ledgerReadFile = func(string) ([]byte, error) { return nil, errors.New("read") }
	if _, _, err := ledger.read(authProviderOpenAI); err == nil {
		t.Fatal("a read failure was swallowed")
	}

	ledgerReadFile = func(string) ([]byte, error) { return []byte("{"), nil }
	if _, _, err := ledger.read(authProviderOpenAI); err == nil {
		t.Fatal("a malformed entry was accepted")
	}
}

func TestAuthLedgerWriteFailures(t *testing.T) {
	ledger := newTestLedger(t)
	record := sampleLedgerRecord()

	cases := map[string]func(){
		"marshal": func() {
			ledgerMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal") }
		},
		"createtemp": func() {
			ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errors.New("temp") }
		},
		"write": func() {
			ledgerCreateTemp = func(string, string) (ledgerFile, error) {
				return &fakeLedgerFile{name: "temp", writeErr: errors.New("write")}, nil
			}
			ledgerRemove = func(string) error { return nil }
		},
		"chmod": func() {
			ledgerCreateTemp = func(string, string) (ledgerFile, error) {
				return &fakeLedgerFile{name: "temp", chmodErr: errors.New("chmod")}, nil
			}
			ledgerRemove = func(string) error { return nil }
		},
		"sync": func() {
			ledgerCreateTemp = func(string, string) (ledgerFile, error) {
				return &fakeLedgerFile{name: "temp", syncErr: errors.New("sync")}, nil
			}
			ledgerRemove = func(string) error { return nil }
		},
		"rename": func() {
			ledgerCreateTemp = func(string, string) (ledgerFile, error) {
				return &fakeLedgerFile{name: "temp"}, nil
			}
			ledgerRename = func(string, string) error { return errors.New("rename") }
			ledgerRemove = func(string) error { return nil }
		},
		"syncdir": func() {
			ledgerOpen = func(string) (*os.File, error) { return nil, errors.New("open") }
		},
	}

	for name, install := range cases {
		t.Run(name, func(t *testing.T) {
			restoreLedgerHooks(t)
			install()

			if err := ledger.write(record); err == nil {
				t.Fatal("a failed write reported success")
			}
		})
	}
}

func TestAuthLedgerListFailures(t *testing.T) {
	ledger := newTestLedger(t)

	if err := ledger.write(sampleLedgerRecord()); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(ledger.dir, "nested"), authLedgerDirMode); err != nil {
		t.Fatalf("seed nested dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(ledger.dir, "note.txt"), []byte("ignored"), authLedgerFileMode); err != nil {
		t.Fatalf("seed non-entry: %v", err)
	}

	records, err := ledger.list()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(records) != 1 {
		t.Fatalf("list returned %d records, want 1", len(records))
	}

	restoreLedgerHooks(t)

	ledgerReadFile = func(string) ([]byte, error) { return nil, errors.New("read") }
	if _, err := ledger.list(); err == nil {
		t.Fatal("a failed entry read was swallowed")
	}

	ledgerReadFile = func(string) ([]byte, error) { return []byte("{"), nil }
	if _, err := ledger.list(); err == nil {
		t.Fatal("a malformed entry was accepted")
	}

	ledgerReadDir = func(string) ([]fs.DirEntry, error) { return nil, errors.New("readdir") }
	if _, err := ledger.list(); err == nil {
		t.Fatal("a failed directory read was swallowed")
	}
}

func TestAuthLedgerListSortsByProvider(t *testing.T) {
	ledger := newTestLedger(t)

	for _, provider := range []string{"zed", "alpha"} {
		record := sampleLedgerRecord()
		record.ProviderID = provider

		if err := ledger.write(record); err != nil {
			t.Fatalf("write %s: %v", provider, err)
		}
	}

	records, err := ledger.list()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(records) != 2 || records[0].ProviderID != "alpha" {
		t.Fatalf("list = %+v", records)
	}
}

func TestAuthProofSourceIsTotal(t *testing.T) {
	cases := []struct {
		state   string
		present bool
		want    string
	}{
		{authLedgerConfirmed, true, authProofConfirmedPresent},
		{authLedgerConfirmed, false, authProofConfirmedAbsent},
		{authLedgerIntent, true, authProofNotConfirmed},
		{authLedgerIntent, false, authProofNotConfirmed},
		{authLedgerRemoved, true, authProofNotConfirmed},
	}

	for _, testCase := range cases {
		if got := authProofSource(testCase.state, testCase.present); got != testCase.want {
			t.Fatalf("%s/%v = %s, want %s", testCase.state, testCase.present, got, testCase.want)
		}
	}
}

func TestAuthInventoryReportsLedgerAndProbe(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

	record := sampleLedgerRecord()
	if err := fixture.broker.ledger.write(record); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	removed := sampleLedgerRecord()
	removed.ProviderID = "gone"
	removed.State = authLedgerRemoved

	if err := fixture.broker.ledger.write(removed); err != nil {
		t.Fatalf("seed removed ledger entry: %v", err)
	}

	result, err := fixture.call(t, AuthInventoryMethod, map[string]any{"sessionId": fixture.sessionID})
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}

	inventory, _ := result.(authInventoryResult)
	entries := inventory.Entries
	if len(entries) != 1 {
		t.Fatalf("inventory returned %d entries, want 1", len(entries))
	}

	if entries[0].ProofSource != authProofConfirmedPresent {
		t.Fatalf("proofSource = %s", entries[0].ProofSource)
	}

	if entries[0].ConnectionID != record.ConnectionID || entries[0].Revision != record.Revision {
		t.Fatalf("entry = %+v", entries[0])
	}
}

func TestAuthInventoryReportsAbsenceWhenTheAccountIsEmpty(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	if err := fixture.broker.ledger.write(sampleLedgerRecord()); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	result, err := fixture.call(t, AuthInventoryMethod, map[string]any{"sessionId": fixture.sessionID})
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}

	inventory, _ := result.(authInventoryResult)
	entries := inventory.Entries
	if entries[0].ProofSource != authProofConfirmedAbsent {
		t.Fatalf("proofSource = %s", entries[0].ProofSource)
	}
}

func TestAuthInventoryFailures(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	if err := fixture.broker.ledger.write(sampleLedgerRecord()); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	fixture.client.accountErr = errors.New("read")

	_, err := fixture.call(t, AuthInventoryMethod, map[string]any{"sessionId": fixture.sessionID})
	if err == nil {
		t.Fatal("a failed probe was swallowed")
	}

	requireAuthCause(t, err, authCauseHarvestFailed)

	restoreLedgerHooks(t)

	ledgerReadDir = func(string) ([]fs.DirEntry, error) { return nil, errors.New("readdir") }

	_, err = fixture.call(t, AuthInventoryMethod, map[string]any{"sessionId": fixture.sessionID})
	if err == nil {
		t.Fatal("a failed ledger list was swallowed")
	}

	requireAuthCause(t, err, authCauseHarvestFailed)
}

func TestAuthInventoryFailsWithoutTheNativeAuthSurface(t *testing.T) {
	home := t.TempDir()
	agent := NewAgent(WithHome(home), WithProviderAuthRoot(t.TempDir()), WithProviderAuthDirectHome(home))
	storeRateLimitsSession(t, agent, "plain", newSpyCodexClient())

	if err := agent.providerAuth.ledger.write(sampleLedgerRecord()); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	_, err := agent.HandleExtensionMethod(t.Context(), AuthInventoryMethod, json.RawMessage(`{"sessionId":"plain"}`))
	if err == nil {
		t.Fatal("inventory answered without the native auth surface")
	}

	requireAuthCause(t, err, authCauseTransport)
}
