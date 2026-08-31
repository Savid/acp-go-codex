//go:build linux

package codex

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsolatedPackagedCodexStageIsTheOnlySharedRuntimeMaterial(t *testing.T) {
	requireTwoPrincipalHarness(t)

	fixture := newPackagedCodexFixture(t)
	require.NoError(t, os.WriteFile(fixture.executable, []byte("#!/bin/sh\nprintf codex-isolated\n"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(fixture.packagePath, "rg"),
		[]byte("#!/bin/sh\nprintf rg-isolated\n"),
		0o700,
	))
	resource := filepath.Join(fixture.root, codexPackageResources, "nested", "fixture.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(resource), 0o755))
	require.NoError(t, os.WriteFile(resource, []byte("resource-isolated"), 0o600))

	parent := testTraversableTempDir(t)
	privateRuntime := filepath.Join(parent, "acp-go-codex-runtime-private")
	require.NoError(t, os.Mkdir(privateRuntime, 0o700))
	privateMaterial := filepath.Join(privateRuntime, "supervisor-private")
	require.NoError(t, os.WriteFile(privateMaterial, []byte("must-stay-private\n"), 0o600))

	isolation := testProcessIsolation()
	staged, env, ownedStageRoot, err := stagePackagedCodexForProcess(
		fixture.executable,
		[]string{"PATH=/usr/bin:/bin"},
		privateRuntime,
		parent,
		isolation,
	)
	require.NoError(t, err)
	require.Equal(t, filepath.Dir(filepath.Dir(staged)), ownedStageRoot)
	require.Equal(t, filepath.Join(parent, filepath.Base(ownedStageRoot)), ownedStageRoot)
	require.NotEqual(t, privateRuntime, ownedStageRoot)

	requireOwnedMode(t, ownedStageRoot, isolation.UID, isolation.GID, 0o700)
	requireOwnedMode(t, filepath.Dir(staged), isolation.UID, isolation.GID, 0o700)
	requireOwnedMode(t, staged, isolation.UID, isolation.GID, 0o700)
	requireOwnedMode(t, privateRuntime, uint32(os.Geteuid()), uint32(os.Getegid()), 0o700)
	requireOwnedMode(t, privateMaterial, uint32(os.Geteuid()), uint32(os.Getegid()), 0o600)

	require.Equal(t, "codex-isolated", runAsIdentity(t, staged, isolation.UID, isolation.GID, env))
	require.Equal(t, "rg-isolated", runAsIdentity(
		t,
		filepath.Join(filepath.Dir(staged), "rg"),
		isolation.UID,
		isolation.GID,
		env,
	))
	require.Equal(t, "resource-isolated", runAsIdentity(
		t,
		"/bin/cat",
		isolation.UID,
		isolation.GID,
		[]string{"PATH=/usr/bin:/bin"},
		filepath.Join(ownedStageRoot, codexPackageResources, "nested", "fixture.txt"),
	))

	privateRead := commandAsIdentity("/bin/cat", isolation.UID, isolation.GID, []string{"PATH=/usr/bin:/bin"}, privateMaterial)
	_, privateReadErr := privateRead.CombinedOutput()
	require.Error(t, privateReadErr, "isolated identity read private runtime material")

	peerUID, peerGID := isolation.UID-1, isolation.GID-1
	if peerUID == 0 || peerUID == uint32(os.Geteuid()) {
		peerUID = isolation.UID + 1
	}
	if peerGID == 0 || peerGID == uint32(os.Getegid()) {
		peerGID = isolation.GID + 1
	}
	peer := commandAsIdentity(staged, peerUID, peerGID, []string{"PATH=/usr/bin:/bin"})
	_, peerErr := peer.CombinedOutput()
	require.Error(t, peerErr, "unrelated identity executed the isolated package stage")

	proc := &process{packageStageRoot: ownedStageRoot}
	require.NoError(t, proc.Close())
	require.NoDirExists(t, ownedStageRoot)
	require.FileExists(t, privateMaterial)
}

func requireOwnedMode(t *testing.T, path string, uid uint32, gid uint32, mode os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.Equal(t, uid, stat.Uid)
	require.Equal(t, gid, stat.Gid)
	require.Equal(t, mode, info.Mode().Perm())
}

func runAsIdentity(t *testing.T, path string, uid uint32, gid uint32, env []string, args ...string) string {
	t.Helper()

	output, err := commandAsIdentity(path, uid, gid, env, args...).CombinedOutput()
	require.NoError(t, err, string(output))

	return string(output)
}

func commandAsIdentity(path string, uid uint32, gid uint32, env []string, args ...string) *exec.Cmd {
	cmd := exec.Command(path, args...)
	cmd.Dir = "/"
	cmd.Env = append([]string(nil), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uid, Gid: gid, Groups: []uint32{}, NoSetGroups: false,
	}}

	return cmd
}
