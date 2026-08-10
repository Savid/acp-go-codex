//go:build linux

package codex

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
	"github.com/stretchr/testify/require"
)

// TestVerifyLinuxTrustedSupervisorIdentityAcceptsTheIdentityItAlreadyRuns
// proves the root assertion is skipped for the one shape it cannot judge and
// for no other. The supervisor demands the trusted root identity because it
// descends from it to reach the native one; when the native identity is the
// identity it already holds there is no descent, so there is nothing to assert.
// Every other shape keeps the refusal it has today, byte for byte, including an
// unprivileged supervisor asked for a target it cannot become.
func TestVerifyLinuxTrustedSupervisorIdentityAcceptsTheIdentityItAlreadyRuns(t *testing.T) {
	linuxSupervisorIdentitySeams(t)
	sharedIdentitySeams(t)

	effectiveUIDSource = func() int { return 1000 }

	processEffectiveUID = func() int { return 1000 }
	require.NoError(t, verifyLinuxTrustedSupervisorIdentity(1000))

	// The identity gate reads its own euid through effectiveUIDSource, which the
	// existing cases fake. The arm must not be selectable that way, or those
	// cases would stop reaching the refusal they pin: the same faked euid and
	// the same target uid now describe the shape the gate has always refused.
	processEffectiveUID = func() int { return 0 }
	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(1000), linuxSupervisorTrustedIdentityMessage)
	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(0), linuxSupervisorTrustedIdentityMessage)
}

// TestSharedIdentityGuardianTakesNoAgentIdentityAuthority proves the durable
// authority is not merely allowed to fail but never attempted. The claim proves
// an identity vacant across every live task in the namespace, and a supervisor
// running as that identity is one of them, so the claim it would take is one it
// can never prove. The guardian carries an empty authority instead, which the
// placeholder descriptors and the liveness leg already know how to handle.
func TestSharedIdentityGuardianTakesNoAgentIdentityAuthority(t *testing.T) {
	sharedIdentitySeams(t)
	processEffectiveUID = func() int { return 1000 }

	identity, authority, err := acquireLinuxAgentIdentityAuthority(1000, 1000, "", "", nil)
	require.NoError(t, err)
	require.Nil(t, identity.InheritedFile())
	require.Nil(t, authority.InheritedFile())
	require.NoError(t, identity.Close())
	require.NoError(t, authority.Close())
}

// TestSharedIdentityMarkerRootIsThePrivateScratchRoot proves the containment
// markers move to storage the supervisor owns. The proof namespace under /run
// is root-owned storage bootstrapped from privilege this deployment never had;
// the markers themselves are O_EXCL 0600 files, and the adapter's own 0700
// scratch root holds them under exactly the terms every platform without the
// namespace already uses.
func TestSharedIdentityMarkerRootIsThePrivateScratchRoot(t *testing.T) {
	sharedIdentitySeams(t)
	processEffectiveUID = func() int { return 1000 }

	scratch := t.TempDir()
	root, err := linuxSupervisorMarkerRoot(supervisorConfig{IsolationUID: 1000, Scratch: scratch})
	require.NoError(t, err)
	require.Equal(t, scratch, root)
}

// TestSupervisorCommandStampsTheIdentityDispositionItRunsUnder proves the
// parent records the arm it selected in the sealed config rather than leaving
// each child to guess. The stamp carries no capability flags and no standalone
// authority, because the shared arm writes neither, and the isolated fixture
// still stamps the standalone authority it has always stamped.
func TestSupervisorCommandStampsTheIdentityDispositionItRunsUnder(t *testing.T) {
	linuxSupervisorIdentitySeams(t)
	sharedIdentitySeams(t)

	writeConfig := supervisorWriteConfig
	t.Cleanup(func() { supervisorWriteConfig = writeConfig })

	var stamped supervisorConfig

	supervisorWriteConfig = func(root string, config supervisorConfig) (*os.File, error) {
		stamped = config

		return writeConfig(root, config)
	}

	scratch := t.TempDir()
	environment := map[string]string{"PATH": "/usr/bin:/bin"}

	processEffectiveUID = func() int { return 1000 }
	_, proof, err := supervisorCommand(context.Background(), supervisorConfig{
		NativePath: "/bin/true", Home: filepath.Join(scratch, "home"), Scratch: scratch,
		Isolation: &ProcessIsolation{UID: 1000, GID: 1000, BaseEnvironment: environment},
	})
	require.NoError(t, err)
	require.NoError(t, proof.closeInherited())
	require.True(t, stamped.SharedIdentity)
	require.False(t, stamped.StandaloneAuthority)
	require.False(t, stamped.IdentityLock)
	require.False(t, stamped.AuthorityDomain)
	require.Equal(t, scratch, filepath.Dir(stamped.Started))
	require.NoError(t, validateSupervisorIdentityDisposition(stamped))

	isolation := testProcessIsolation()
	processEffectiveUID = func() int { return 0 }
	effectiveUIDSource = func() int { return 0 }
	_, proof, err = supervisorCommand(context.Background(), supervisorConfig{
		NativePath: "/bin/true", Home: filepath.Join(scratch, "home"), Scratch: scratch,
		Isolation: isolation,
	})
	if err == nil {
		require.NoError(t, proof.closeInherited())
		require.False(t, stamped.SharedIdentity)
		require.True(t, stamped.StandaloneAuthority)
		require.NoError(t, validateSupervisorIdentityDisposition(stamped))

		return
	}

	// Off a trusted root supervisor the isolated leg cannot reach the stamp; the
	// refusal it stops at is the one the arm must leave standing.
	require.EqualError(t, err, linuxSupervisorTrustedIdentityMessage)
}

// TestSupervisorIdentityDispositionRefusesAConfigItsIdentityContradicts proves
// the stamp is cross-checked rather than trusted. Each child re-derives the arm
// from the identity it actually runs as, so a config claiming an identity the
// child does not hold, or denying one it does, is refused before anything is
// launched. The shared stamp additionally promises no authority record, and a
// config carrying one is refused with it.
func TestSupervisorIdentityDispositionRefusesAConfigItsIdentityContradicts(t *testing.T) {
	sharedIdentitySeams(t)
	processEffectiveUID = func() int { return 1000 }

	const disagreement = "codex supervisor identity disposition does not match the identity it runs as"

	require.NoError(t, validateSupervisorIdentityDisposition(supervisorConfig{IsolationUID: 1000, SharedIdentity: true}))
	require.NoError(t, validateSupervisorIdentityDisposition(supervisorConfig{
		IsolationUID: 65534, StandaloneAuthority: true, StandaloneOwnerID: "test-owner",
	}))

	require.EqualError(t,
		validateSupervisorIdentityDisposition(supervisorConfig{IsolationUID: 65534, SharedIdentity: true}),
		disagreement,
	)
	require.EqualError(t,
		validateSupervisorIdentityDisposition(supervisorConfig{IsolationUID: 1000}),
		disagreement,
	)

	for name, config := range map[string]supervisorConfig{
		"identity lock":        {IsolationUID: 1000, SharedIdentity: true, IdentityLock: true},
		"authority domain":     {IsolationUID: 1000, SharedIdentity: true, AuthorityDomain: true},
		"standalone authority": {IsolationUID: 1000, SharedIdentity: true, StandaloneAuthority: true},
		"owner id":             {IsolationUID: 1000, SharedIdentity: true, StandaloneOwnerID: "deployment-1"},
		"state root":           {IsolationUID: 1000, SharedIdentity: true, StandaloneStateRoot: "/var/lib/acp-go-codex"},
	} {
		t.Run(name, func(t *testing.T) {
			require.EqualError(t,
				validateSupervisorIdentityDisposition(config),
				"codex shared supervisor identity disposition is invalid",
			)
		})
	}
}

func TestRunSupervisorPropagatesADispositionRefusal(t *testing.T) {
	sharedIdentitySeams(t)
	processEffectiveUID = func() int { return 4242 }

	config := supervisorConfig{
		NativePath:     "/bin/true",
		Home:           "home",
		Scratch:        t.TempDir(),
		IsolationUID:   1000,
		IsolationGID:   1000,
		SharedIdentity: true,
	}

	path, err := writeSupervisorConfig(config.Scratch, config)
	require.NoError(t, err)

	require.EqualError(t,
		runSupervisor(supervisorModeLiveness, path),
		"codex supervisor identity disposition does not match the identity it runs as",
	)
}

// TestSharedIdentitySupervisorContainsARealNativeLaunch is the end-to-end proof
// with no seams at all: the guardian and liveness pair self-exec, claim the home
// locks, arm the subreaper, run a real native process under the identity the
// test itself holds, complete the readiness handshake, forward its output, and
// prove the tree gone before publishing completion. Nothing here is available to
// a trusted root supervisor, which is why the case skips there.
func TestSharedIdentitySupervisorContainsARealNativeLaunch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the shared arm is unreachable for a trusted root supervisor")
	}

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
		Isolation:  &ProcessIsolation{UID: uid, GID: gid, BaseEnvironment: environmentMap(nativeEnv)},
	})
	require.NoError(t, err)

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

	// The whole handshake ran inside the private scratch root, and the proof
	// files are removed once the guardian has read them.
	require.Equal(t, scratch, filepath.Dir(proof.completion))
	require.NoFileExists(t, proof.completion)
	require.NoFileExists(t, proof.started)

	lock, err := homelock.Acquire(home)
	require.NoError(t, err)
	require.NoError(t, lock.Release())

	require.NotEqual(t, syscall.Getpid(), nativePID)
}
