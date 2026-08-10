//go:build linux

package codex

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
	"github.com/stretchr/testify/require"
)

// ordinaryUserHelperEnv marks the re-exec'd half of the ordinary-execution
// case, where a privileged runner runs this same test binary as an unprivileged
// identity. It carries the canonical internal-helper suffix the family reserves
// for private self-exec test plumbing.
const ordinaryUserHelperEnv = "ACP_GO_CODEX_INTERNAL_HELPER"

func TestExplicitProcessIsolationRequiresDistinctTrustedRoot(t *testing.T) {
	linuxSupervisorIdentitySeams(t)

	effectiveUIDSource = func() int { return 1000 }
	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(1000), linuxSupervisorTrustedIdentityMessage)
	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(0), linuxSupervisorTrustedIdentityMessage)
	if os.Geteuid() == 0 {
		require.NoError(t, verifyLinuxTrustedSupervisorIdentity(65534))
	} else {
		require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(65534), linuxSupervisorTrustedIdentityMessage)
	}
}

func TestOrdinaryLinuxSupervisorSealsCurrentRootAndNonRootIdentity(t *testing.T) {
	processIdentitySeams(t)

	originalWriteConfig := supervisorWriteConfig
	t.Cleanup(func() { supervisorWriteConfig = originalWriteConfig })

	for _, testCase := range []struct {
		name string
		uid  int
		gid  int
	}{
		{name: "root", uid: 0, gid: 0},
		{name: "nonroot", uid: 1000, gid: 1001},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			processEffectiveUID = func() int { return testCase.uid }
			processEffectiveGID = func() int { return testCase.gid }

			var stamped supervisorConfig
			supervisorWriteConfig = func(root string, config supervisorConfig) (*os.File, error) {
				stamped = config

				return originalWriteConfig(root, config)
			}

			scratch := t.TempDir()
			_, proof, err := supervisorCommand(context.Background(), supervisorConfig{
				NativePath: "/bin/true",
				NativeEnv:  []string{"PATH=/usr/bin:/bin"},
				Home:       filepath.Join(scratch, "home"),
				Scratch:    scratch,
			})
			require.NoError(t, err)
			require.NotNil(t, proof)
			require.NoError(t, proof.closeInherited())
			require.True(t, stamped.OrdinaryExecution)
			require.Equal(t, uint32(testCase.uid), stamped.IsolationUID)
			require.Equal(t, uint32(testCase.gid), stamped.IsolationGID)
			require.False(t, stamped.IdentityLock)
			require.False(t, stamped.AuthorityDomain)
			require.False(t, stamped.StandaloneAuthority)
			require.Empty(t, stamped.StandaloneOwnerID)
			require.Empty(t, stamped.StandaloneStateRoot)
			require.Empty(t, stamped.ProviderSnapshot)
			require.Equal(t, scratch, filepath.Dir(stamped.Started))
			require.NoError(t, validateSupervisorIdentityDisposition(stamped))
		})
	}
}

func TestSupervisorOrdinaryIdentityDispositionRefusesContradictions(t *testing.T) {
	processIdentitySeams(t)
	processEffectiveUID = func() int { return 1000 }
	processEffectiveGID = func() int { return 1001 }

	require.NoError(t, validateSupervisorIdentityDisposition(supervisorConfig{
		OrdinaryExecution: true,
		IsolationUID:      1000,
		IsolationGID:      1001,
	}))
	require.NoError(t, validateSupervisorIdentityDisposition(supervisorConfig{
		IsolationUID: 65534,
		IsolationGID: 65534,
	}))
	require.Error(t, validateSupervisorIdentityDisposition(supervisorConfig{
		OrdinaryExecution: true,
		IsolationUID:      1000,
		IsolationGID:      1002,
	}))
}

func TestProcessIsolationOmissionAllowsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires a root caller to prove ordinary root execution")
	}

	runOrdinaryIdentityLaunch(t)
}

func TestProcessIsolationOmissionAllowsOrdinaryUser(t *testing.T) {
	if os.Getenv(ordinaryUserHelperEnv) == "1" {
		require.NotZero(t, os.Geteuid())
		runOrdinaryIdentityLaunch(t)

		return
	}

	if os.Geteuid() != 0 {
		runOrdinaryIdentityLaunch(t)

		return
	}

	uid, gid := testIsolationIdentity()

	// t.TempDir nests its leaf under a 0700 parent, so the unprivileged identity
	// cannot traverse down to the helper it is asked to exec. The traversable
	// root is flat and 0711, which is the only shape that ancestry admits.
	root := testTraversableTempDir(t)
	helper := filepath.Join(root, "ordinary-user.test")
	binary, err := os.ReadFile(os.Args[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(helper, binary, 0o755))

	// The re-exec'd half calls t.TempDir itself, and the suite's TMPDIR is a
	// root-owned tmpfs the unprivileged identity cannot create in. It gets its
	// own temporary root, owned by the identity it will run as.
	childTemp := filepath.Join(root, "tmp")
	require.NoError(t, os.Mkdir(childTemp, 0o700))
	require.NoError(t, os.Chown(childTemp, int(uid), int(gid)))

	cmd := exec.Command(helper, "-test.run=^TestProcessIsolationOmissionAllowsOrdinaryUser$")
	cmd.Env = append(os.Environ(), ordinaryUserHelperEnv+"=1", "TMPDIR="+childTemp)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uid, Gid: gid, Groups: []uint32{},
	}}
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func runOrdinaryIdentityLaunch(t *testing.T) {
	t.Helper()

	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o711))
	home := filepath.Join(root, "home")
	scratch := filepath.Join(root, "scratch")
	require.NoError(t, os.MkdirAll(scratch, 0o700))
	nativePIDPath := filepath.Join(root, "native.pid")
	script := filepath.Join(root, "native.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/bin/sh
echo "$$" > "$NATIVE_PID_FILE"
printf 'identity %s:%s\n' "$(id -u)" "$(id -g)"
while IFS= read -r line; do
  [ "$line" = "exit" ] && exit 0
  printf '%s\n' "$line"
done
`), 0o700))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nativeEnv := append(os.Environ(), "NATIVE_PID_FILE="+nativePIDPath)
	cmd, proof, err := supervisorCommand(ctx, supervisorConfig{
		NativePath: script,
		NativeEnv:  nativeEnv,
		Home:       home,
		Scratch:    scratch,
	})
	require.NoError(t, err)
	require.Nil(t, cmd.SysProcAttr)
	require.Empty(t, proof.providerSnapshot)

	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stderr := new(supervisorStderr)
	cmd.Stderr = stderr

	waiter, err := startProcess(cmd)
	require.NoError(t, err)
	require.NoError(t, proof.closeInherited())
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	require.NoError(t, err, "supervisor stderr: %s", stderr.String())
	require.Equal(t,
		"identity "+strconv.FormatUint(uint64(uid), 10)+":"+strconv.FormatUint(uint64(gid), 10),
		strings.TrimSpace(line),
	)

	_, err = io.WriteString(stdin, "round trip\n")
	require.NoError(t, err)
	line, err = reader.ReadString('\n')
	require.NoError(t, err, "supervisor stderr: %s", stderr.String())
	require.Equal(t, "round trip", strings.TrimSpace(line))

	nativePID := waitPIDFile(t, nativePIDPath, stderr)
	_, err = homelock.Acquire(home)
	require.Error(t, err, "ordinary launch did not retain writable-home exclusion")

	_, err = io.WriteString(stdin, "exit\n")
	require.NoError(t, err)
	require.NoError(t, stdin.Close())
	waiter.start()

	select {
	case waitErr := <-waiter.result():
		require.NoError(t, waitErr, "supervisor stderr: %s", stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("supervisor did not exit; stderr=%s", stderr.String())
	}

	require.NoError(t, proof.awaitCompletion())
	assertProcessGone(t, nativePID)
	require.Equal(t, scratch, filepath.Dir(proof.completion))
	require.NoFileExists(t, proof.completion)
	require.NoFileExists(t, proof.started)

	lock, err := homelock.Acquire(home)
	require.NoError(t, err)
	require.NoError(t, lock.Release())
	require.NotEqual(t, os.Getpid(), nativePID)
}
