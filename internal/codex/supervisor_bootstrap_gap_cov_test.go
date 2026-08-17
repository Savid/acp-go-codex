package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// bootstrapGapSeams captures every supervisor seam the cases in this file swap
// and restores each one when the case ends, so one case can never decide the
// answer another case gets.
func bootstrapGapSeams(t *testing.T) {
	t.Helper()

	executable := supervisorExecutable
	command := supervisorExecCommand
	chmod := supervisorChmod
	createTemp := supervisorCreateTemp
	encode := supervisorEncodeConfig
	inheritedFile := supervisorInheritedFile
	writeConfig := supervisorWriteConfig
	markerRoot := supervisorMarkerRoot
	verifyIdentity := supervisorVerifyTrustedIdentity
	acquireAuthority := supervisorAcquireIdentityAuthority
	adoptLock := supervisorAdoptIdentityLock
	adoptDomain := supervisorAdoptAuthorityDomain
	validateAdopted := supervisorValidateAdoptedAuthority
	validatePeer := supervisorValidateGuardianPeer
	openLiveness := supervisorOpenLivenessContainment
	guardianPeer := supervisorGuardianPeer
	input := supervisorInput
	output := supervisorOutput
	errorOutput := supervisorError
	exit := supervisorExit

	t.Cleanup(func() {
		supervisorExecutable = executable
		supervisorExecCommand = command
		supervisorChmod = chmod
		supervisorCreateTemp = createTemp
		supervisorEncodeConfig = encode
		supervisorInheritedFile = inheritedFile
		supervisorWriteConfig = writeConfig
		supervisorMarkerRoot = markerRoot
		supervisorVerifyTrustedIdentity = verifyIdentity
		supervisorAcquireIdentityAuthority = acquireAuthority
		supervisorAdoptIdentityLock = adoptLock
		supervisorAdoptAuthorityDomain = adoptDomain
		supervisorValidateAdoptedAuthority = validateAdopted
		supervisorValidateGuardianPeer = validatePeer
		supervisorOpenLivenessContainment = openLiveness
		supervisorGuardianPeer = guardianPeer
		supervisorInput = input
		supervisorOutput = output
		supervisorError = errorOutput
		supervisorExit = exit
	})
}

// bootstrapGapIdentityLock counts the releases the adopted-identity arms of
// runSupervisor owe through their deferred closes.
type bootstrapGapIdentityLock struct{ closed *int }

func (lock bootstrapGapIdentityLock) Close() error {
	*lock.closed++

	return nil
}

func (bootstrapGapIdentityLock) InheritedFile() *os.File { return nil }

// bootstrapGapCapability is a duplicable process identity capability whose
// duplication outcome the case selects.
type bootstrapGapCapability struct{ err error }

func (capability bootstrapGapCapability) Duplicate() (*os.File, error) {
	if capability.err != nil {
		return nil, capability.err
	}

	return os.Open(os.DevNull)
}

// bootstrapGapBorrowedIsolation is an isolation that carries both borrowed
// identity capabilities. A borrowed identity forbids the standalone owner
// fields, so they are cleared: otherwise the disposition check refuses the
// isolation before supervisorCommand reaches the duplication it is about.
func bootstrapGapBorrowedIsolation(lock, domain ProcessIdentityLockCapability) *ProcessIsolation {
	isolation := testProcessIsolation()
	isolation.StandaloneOwnerID = ""
	isolation.StandaloneStateRoot = ""
	isolation.IdentityLock = lock
	isolation.AuthorityDomain = domain

	return isolation
}

// bootstrapGapMarkerConfig is a private supervisor config complete enough for
// readSupervisorConfig to accept and for a dispatch to begin.
func bootstrapGapMarkerConfig(t *testing.T) supervisorConfig {
	t.Helper()

	root := t.TempDir()

	return supervisorConfig{
		NativePath: "/bin/sh", NativeEnv: os.Environ(),
		Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
		LifecycleKind: lifecycleRuntime, DarwinBestEffort: true,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		NativePIDFile: filepath.Join(root, "pid"),
		IsolationUID:  1, IsolationGID: 2,
	}
}

// bootstrapGapAfter runs one action once the caller's poll loop is certainly
// running, and keeps the helper goroutine joined to the case that started it.
func bootstrapGapAfter(t *testing.T, delay time.Duration, action func()) {
	t.Helper()

	var group sync.WaitGroup

	group.Add(1)

	go func() {
		defer group.Done()

		time.Sleep(delay)
		action()
	}()

	t.Cleanup(group.Wait)
}

// TestWriteSupervisorConfigDescriptorFaults proves every failure arm of the
// private config descriptor writer reports what failed and leaves no readable
// config behind in the scratch root.
func TestWriteSupervisorConfigDescriptorFaults(t *testing.T) {
	t.Run("chmod scratch root", func(t *testing.T) {
		bootstrapGapSeams(t)

		supervisorChmod = func(string, os.FileMode) error { return errors.New("chmod refused") }

		_, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.ErrorContains(t, err, "chmod private supervisor scratch root")
		require.ErrorContains(t, err, "chmod refused")
	})

	t.Run("create config", func(t *testing.T) {
		bootstrapGapSeams(t)

		supervisorCreateTemp = func(string, string) (*os.File, error) {
			return nil, errors.New("temp refused")
		}

		_, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.ErrorContains(t, err, "create private supervisor config")
		require.ErrorContains(t, err, "temp refused")
	})

	t.Run("secure config", func(t *testing.T) {
		bootstrapGapSeams(t)

		var path string

		// A descriptor that cannot be restricted to the supervisor pair is
		// unlinked rather than handed on.
		supervisorCreateTemp = func(dir string, pattern string) (*os.File, error) {
			file, createErr := os.CreateTemp(dir, pattern)
			if createErr != nil {
				return nil, createErr
			}

			path = file.Name()

			return file, file.Close()
		}

		_, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.ErrorContains(t, err, "secure private supervisor config")
		require.NotEmpty(t, path)
		require.NoFileExists(t, path)
	})

	t.Run("rewind config", func(t *testing.T) {
		bootstrapGapSeams(t)

		var path string

		supervisorCreateTemp = func(dir string, pattern string) (*os.File, error) {
			file, createErr := os.CreateTemp(dir, pattern)
			if createErr != nil {
				return nil, createErr
			}

			path = file.Name()

			return file, nil
		}
		// A descriptor the encoder retired cannot be rewound for the child, and
		// a config the child could not read from byte zero is never handed on.
		supervisorEncodeConfig = func(writer io.Writer, _ supervisorConfig) error {
			file, ok := writer.(*os.File)
			require.True(t, ok)

			return file.Close()
		}

		_, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.ErrorContains(t, err, "rewind private supervisor config")
		require.NotEmpty(t, path)
		require.NoFileExists(t, path)
	})

	t.Run("unlink config", func(t *testing.T) {
		bootstrapGapSeams(t)

		// The config must leave no name behind. A name that is already gone
		// means the writer cannot prove that, so it refuses the descriptor.
		supervisorCreateTemp = func(dir string, pattern string) (*os.File, error) {
			file, createErr := os.CreateTemp(dir, pattern)
			if createErr != nil {
				return nil, createErr
			}

			if removeErr := os.Remove(file.Name()); removeErr != nil {
				_ = file.Close()

				return nil, removeErr
			}

			return file, nil
		}

		_, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.ErrorContains(t, err, "unlink private supervisor config")
	})
}

// TestSupervisorCommandPreparationFaults proves each refusal supervisorCommand
// owes before it hands a guardian its descriptors, and that a refused
// preparation returns no command and no proof for a caller to act on.
func TestSupervisorCommandPreparationFaults(t *testing.T) {
	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		cmd, proof, err := supervisorCommand(ctx, supervisorConfig{
			Scratch: t.TempDir(), Isolation: testProcessIsolation(),
		})
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, cmd)
		require.Nil(t, proof)
	})

	t.Run("untrusted identity", func(t *testing.T) {
		bootstrapGapSeams(t)

		supervisorVerifyTrustedIdentity = func(uint32) error { return errors.New("untrusted identity") }

		cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
			Scratch: t.TempDir(), Isolation: testProcessIsolation(),
		})
		require.ErrorContains(t, err, "untrusted identity")
		require.Nil(t, cmd)
		require.Nil(t, proof)
	})

	t.Run("marker root", func(t *testing.T) {
		bootstrapGapSeams(t)

		supervisorVerifyTrustedIdentity = func(uint32) error { return nil }
		supervisorMarkerRoot = func(supervisorConfig) (string, error) {
			return "", errors.New("no marker namespace")
		}

		cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
			Scratch: t.TempDir(), Isolation: testProcessIsolation(),
		})
		require.ErrorContains(t, err, "no marker namespace")
		require.Nil(t, cmd)
		require.Nil(t, proof)
	})

	t.Run("write config", func(t *testing.T) {
		bootstrapGapSeams(t)

		markerRoot := t.TempDir()
		supervisorVerifyTrustedIdentity = func(uint32) error { return nil }
		supervisorMarkerRoot = func(supervisorConfig) (string, error) { return markerRoot, nil }
		supervisorWriteConfig = func(string, supervisorConfig) (*os.File, error) {
			return nil, errors.New("config descriptor refused")
		}

		cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
			Scratch: t.TempDir(), Isolation: testProcessIsolation(),
		})
		require.ErrorContains(t, err, "config descriptor refused")
		require.Nil(t, cmd)
		require.Nil(t, proof)
	})

	t.Run("executable policy", func(t *testing.T) {
		bootstrapGapSeams(t)

		markerRoot := t.TempDir()
		supervisorVerifyTrustedIdentity = func(uint32) error { return nil }
		supervisorMarkerRoot = func(supervisorConfig) (string, error) { return markerRoot, nil }
		// The guardian environment carries only the mode variable, so a bare
		// name has no search path the process policy will accept.
		supervisorExecutable = func() (string, error) { return "acp-go-codex", nil }

		cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
			Scratch: t.TempDir(), Isolation: testProcessIsolation(),
		})
		require.ErrorContains(t, err, "resolve embedded runtime supervisor through process policy")
		require.Nil(t, cmd)
		require.Nil(t, proof)
	})

	for name, capabilities := range map[string]struct {
		lock    ProcessIdentityLockCapability
		domain  ProcessIdentityLockCapability
		message string
	}{
		"identity lock duplicate": {
			lock:    bootstrapGapCapability{err: errors.New("lock duplicate refused")},
			domain:  bootstrapGapCapability{},
			message: "duplicate Codex agent identity lock",
		},
		"authority domain duplicate": {
			lock:    bootstrapGapCapability{},
			domain:  bootstrapGapCapability{err: errors.New("domain duplicate refused")},
			message: "duplicate Codex agent authority domain",
		},
	} {
		t.Run(name, func(t *testing.T) {
			bootstrapGapSeams(t)

			markerRoot := t.TempDir()
			supervisorVerifyTrustedIdentity = func(uint32) error { return nil }
			supervisorMarkerRoot = func(supervisorConfig) (string, error) { return markerRoot, nil }
			supervisorExecutable = func() (string, error) { return "/bin/sh", nil }

			cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
				NativePath: "/bin/sh", Home: filepath.Join(t.TempDir(), "home"), Scratch: t.TempDir(),
				Isolation: bootstrapGapBorrowedIsolation(capabilities.lock, capabilities.domain),
			})
			require.ErrorContains(t, err, capabilities.message)
			require.ErrorContains(t, err, "duplicate refused")
			require.Nil(t, cmd)
			require.Nil(t, proof)
		})
	}

	t.Run("borrowed capabilities inherited", func(t *testing.T) {
		bootstrapGapSeams(t)

		markerRoot := t.TempDir()
		supervisorVerifyTrustedIdentity = func(uint32) error { return nil }
		supervisorMarkerRoot = func(supervisorConfig) (string, error) { return markerRoot, nil }
		supervisorExecutable = func() (string, error) { return "/bin/sh", nil }

		cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
			NativePath: "/bin/sh", Home: filepath.Join(t.TempDir(), "home"), Scratch: t.TempDir(),
			Isolation: bootstrapGapBorrowedIsolation(bootstrapGapCapability{}, bootstrapGapCapability{}),
		})
		require.NoError(t, err)
		// The config descriptor plus both duplicated capabilities reach the
		// guardian, and the proof owns every one of them for teardown.
		require.Len(t, cmd.ExtraFiles, 3)
		require.Len(t, proof.inherited, 3)
		require.Equal(t, markerRoot, filepath.Dir(proof.completion))
		require.NoError(t, proof.closeInherited())
	})
}

// TestSupervisorProofQuarantineStatArms proves the proof wait treats a
// quarantine marker it cannot stat as an incomplete containment rather than as
// an absent marker, in both the start and the completion window.
func TestSupervisorProofQuarantineStatArms(t *testing.T) {
	t.Run("startup window stat", func(t *testing.T) {
		root := t.TempDir()
		blocker := filepath.Join(root, "blocker")
		require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

		err := (&supervisorProof{
			started:    filepath.Join(root, "startup-start"),
			completion: filepath.Join(root, "startup-complete"),
			quarantine: filepath.Join(blocker, "child"),
		}).awaitCompletion()
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, "stat liveness quarantine proof")
	})

	t.Run("completion window stat", func(t *testing.T) {
		root := t.TempDir()
		started := filepath.Join(root, "completion-start")
		require.NoError(t, writeSupervisorMarker(started))

		// The blocker appears only after the start proof has moved the wait
		// into its completion window, so this is the second window's stat.
		blocker := filepath.Join(root, "late-blocker")
		bootstrapGapAfter(t, 50*time.Millisecond, func() {
			_ = os.WriteFile(blocker, []byte("x"), 0o600)
		})

		err := (&supervisorProof{
			started:        started,
			completion:     filepath.Join(root, "completion-complete"),
			quarantine:     filepath.Join(blocker, "child"),
			completionWait: 10 * time.Second,
		}).awaitCompletion()
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, "stat liveness quarantine proof")
	})

	t.Run("completion window quarantine", func(t *testing.T) {
		root := t.TempDir()
		started := filepath.Join(root, "quarantine-start")
		quarantine := filepath.Join(root, "quarantine")
		require.NoError(t, writeSupervisorMarker(started))

		bootstrapGapAfter(t, 50*time.Millisecond, func() {
			_ = writeSupervisorMarker(quarantine)
		})

		err := (&supervisorProof{
			started:        started,
			completion:     filepath.Join(root, "quarantine-complete"),
			quarantine:     quarantine,
			completionWait: 10 * time.Second,
		}).awaitCompletion()
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, "retained the identity lock")
	})
}

// TestSupervisorProofNilGuards proves the marker helpers answer for a proof
// that was never armed instead of dereferencing it.
func TestSupervisorProofNilGuards(t *testing.T) {
	require.NotPanics(t, func() { (*supervisorProof)(nil).removeTerminalMarkers() })

	quarantined, err := (*supervisorProof)(nil).quarantineDetected()
	require.NoError(t, err)
	require.False(t, quarantined)
}

// TestSupervisorAwaitCommandQuarantineArms proves the command wait stops on the
// quarantine marker instead of waiting for a process exit that a quarantined
// liveness supervisor will never deliver.
func TestSupervisorAwaitCommandQuarantineArms(t *testing.T) {
	t.Run("quarantine observed", func(t *testing.T) {
		root := t.TempDir()
		quarantine := filepath.Join(root, "quarantine")
		require.NoError(t, writeSupervisorMarker(quarantine))

		waitErr, proofErr := (&supervisorProof{
			started:    filepath.Join(root, "missing-start"),
			completion: filepath.Join(root, "missing-complete"),
			quarantine: quarantine,
		}).awaitCommand(make(chan error))
		require.NoError(t, waitErr)
		require.ErrorIs(t, proofErr, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, proofErr, "retained the identity lock")
	})

	t.Run("quarantine stat", func(t *testing.T) {
		root := t.TempDir()
		blocker := filepath.Join(root, "blocker")
		require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

		waitErr, proofErr := (&supervisorProof{
			quarantine: filepath.Join(blocker, "child"),
		}).awaitCommand(make(chan error))
		require.NoError(t, waitErr)
		require.ErrorIs(t, proofErr, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, proofErr, "stat liveness quarantine proof")
	})
}

// TestRunSupervisorAdoptedIdentityArms proves the liveness dispatch adopts the
// identity lock and the authority domain together, refuses an inconsistent or
// unprovable adoption, and releases every capability it did adopt on the way
// out.
func TestRunSupervisorAdoptedIdentityArms(t *testing.T) {
	encoded := func(t *testing.T, config supervisorConfig) io.Reader {
		t.Helper()

		raw, err := json.Marshal(config)
		require.NoError(t, err)

		return bytes.NewReader(raw)
	}

	adopted := func(t *testing.T) supervisorConfig {
		t.Helper()

		config := bootstrapGapMarkerConfig(t)
		config.IdentityLock = true
		config.AuthorityDomain = true

		return config
	}

	t.Run("unreadable config", func(t *testing.T) {
		require.ErrorContains(t, runSupervisor(supervisorModeLiveness, nil),
			"missing private supervisor config descriptor")
	})

	t.Run("inconsistent disposition", func(t *testing.T) {
		config := adopted(t)
		config.AuthorityDomain = false

		require.ErrorContains(t, runSupervisor(supervisorModeLiveness, encoded(t, config)),
			"UID lock and authority domain are inconsistent")
	})

	t.Run("adopt lock refused", func(t *testing.T) {
		bootstrapGapSeams(t)

		domains := 0
		supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) {
			return nil, errors.New("adopt lock refused")
		}
		supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) {
			domains++

			return noopSupervisorIdentityLock{}, nil
		}

		require.ErrorContains(t, runSupervisor(supervisorModeLiveness, encoded(t, adopted(t))),
			"adopt lock refused")
		// A dispatch that could not take the lock never reaches for the domain.
		require.Equal(t, 0, domains)
	})

	t.Run("adopt domain refused", func(t *testing.T) {
		bootstrapGapSeams(t)

		closed := 0
		supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) {
			return bootstrapGapIdentityLock{closed: &closed}, nil
		}
		supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) {
			return nil, errors.New("adopt domain refused")
		}

		require.ErrorContains(t, runSupervisor(supervisorModeLiveness, encoded(t, adopted(t))),
			"adopt domain refused")
		// The lock it did adopt is released on the way out.
		require.Equal(t, 1, closed)
	})

	t.Run("adopted authority unproven", func(t *testing.T) {
		bootstrapGapSeams(t)

		closed := 0
		supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) {
			return bootstrapGapIdentityLock{closed: &closed}, nil
		}
		supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) {
			return bootstrapGapIdentityLock{closed: &closed}, nil
		}
		supervisorValidateAdoptedAuthority = func(supervisorConfig) error {
			return errors.New("adopted authority unproven")
		}

		require.ErrorContains(t, runSupervisor(supervisorModeLiveness, encoded(t, adopted(t))),
			"adopted authority unproven")
		// Both adopted capabilities are released before the dispatch gives up.
		require.Equal(t, 2, closed)
	})
}

// TestSupervisorBootstrapDescriptorArms proves the embedded supervisor modes
// refuse to run without the descriptors their role requires, publish the
// inherited guardian peer before dispatching a liveness run, and report the
// dispatch outcome through the process exit status.
func TestSupervisorBootstrapDescriptorArms(t *testing.T) {
	t.Run("missing config descriptor", func(t *testing.T) {
		bootstrapGapSeams(t)
		t.Setenv(supervisorModeEnv, supervisorModeGuardian)

		var reported strings.Builder

		supervisorError = &reported
		supervisorInheritedFile = func(uintptr, string) *os.File { return nil }

		exitCode := -1
		supervisorExit = func(code int) { exitCode = code }

		supervisorBootstrap()
		require.Equal(t, 1, exitCode)
		require.Contains(t, reported.String(), "inherited config descriptor is unavailable")
	})

	t.Run("missing guardian peer descriptor", func(t *testing.T) {
		bootstrapGapSeams(t)
		t.Setenv(supervisorModeEnv, supervisorModeLiveness)

		configFile, err := os.CreateTemp(t.TempDir(), "supervisor-config")
		require.NoError(t, err)
		t.Cleanup(func() { _ = configFile.Close() })

		var reported strings.Builder

		supervisorError = &reported
		supervisorInheritedFile = func(fd uintptr, _ string) *os.File {
			if fd == 3 {
				return configFile
			}

			return nil
		}

		exitCode := -1
		supervisorExit = func(code int) { exitCode = code }

		supervisorBootstrap()
		require.Equal(t, 1, exitCode)
		require.Contains(t, reported.String(), "guardian peer descriptor is unavailable")
	})

	t.Run("liveness publishes the inherited peer", func(t *testing.T) {
		bootstrapGapSeams(t)
		withNeutralSupervisorIdentityHooks(t)
		t.Setenv(supervisorModeEnv, supervisorModeLiveness)

		config := bootstrapGapMarkerConfig(t)
		configFile, err := writeSupervisorConfig(config.Scratch, config)
		require.NoError(t, err)
		t.Cleanup(func() { _ = configFile.Close() })

		peerRead, peerWrite, pipeErr := os.Pipe()
		require.NoError(t, pipeErr)
		t.Cleanup(func() {
			_ = peerWrite.Close()
			_ = peerRead.Close()
		})

		var published *os.File

		supervisorOpenLivenessContainment = func(supervisorConfig) (*livenessContainment, error) {
			published = supervisorGuardianPeer

			return nil, errors.New("containment refused")
		}

		var reported strings.Builder

		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = &reported
		supervisorInheritedFile = func(fd uintptr, _ string) *os.File {
			if fd == 3 {
				return configFile
			}

			return peerRead
		}

		exitCode := -1
		supervisorExit = func(code int) { exitCode = code }

		supervisorBootstrap()
		require.Equal(t, 1, exitCode)
		require.Contains(t, reported.String(), "containment refused")
		// The liveness run sees the descriptor it inherited, not a stale peer.
		require.Same(t, peerRead, published)
	})

	t.Run("guardian run reports success", func(t *testing.T) {
		bootstrapGapSeams(t)
		withNeutralSupervisorIdentityHooks(t)
		t.Setenv(supervisorModeEnv, supervisorModeGuardian)

		config := bootstrapGapMarkerConfig(t)
		require.NoError(t, writeSupervisorMarker(config.Completion))

		configFile, err := writeSupervisorConfig(config.Scratch, config)
		require.NoError(t, err)
		t.Cleanup(func() { _ = configFile.Close() })

		var reported strings.Builder

		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = &reported
		supervisorExecutable = func() (string, error) { return "/bin/sh", nil }
		// A stand-in liveness supervisor that publishes readiness and exits
		// cleanly, leaving the completion proof the guardian requires.
		supervisorExecCommand = func(string, ...string) *exec.Cmd {
			return exec.Command("/bin/sh", "-c",
				"printf '%s\\n' '"+supervisorReadyPrefix+`{"nativePid":4321}`+"' >&2; sleep 0.2; exit 0")
		}
		supervisorInheritedFile = func(uintptr, string) *os.File { return configFile }

		exitCode := -1
		supervisorExit = func(code int) { exitCode = code }

		supervisorBootstrap()
		require.Equal(t, 0, exitCode)
		require.NotContains(t, reported.String(), "runtime supervisor:")
	})
}
