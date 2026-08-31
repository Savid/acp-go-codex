//go:build darwin

package codex

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// preserveSupervisorIdentityGlobals restores the supervisor seams that
// preserveSupervisorGlobals does not own: identity acquisition, the guardian
// peer, the inherited descriptors, and the quarantine retry hooks.
func preserveSupervisorIdentityGlobals(t *testing.T) {
	t.Helper()

	oldVerify := supervisorVerifyTrustedIdentity
	oldAcquire := supervisorAcquireIdentityAuthority
	oldAdoptLock := supervisorAdoptIdentityLock
	oldAdoptDomain := supervisorAdoptAuthorityDomain
	oldValidateAuthority := supervisorValidateAdoptedAuthority
	oldMarkerRoot := supervisorMarkerRoot
	oldPipe := supervisorPipe
	oldPlaceholder := supervisorOpenIdentityPlaceholder
	oldInherited := supervisorInheritedFile
	oldWriteConfig := supervisorWriteConfig
	oldValidatePeer := supervisorValidateGuardianPeer
	oldPeer := supervisorGuardianPeer
	oldQuarantineRetry := supervisorQuarantineRetry
	oldGuardianRetry := supervisorGuardianQuarantineRetry
	oldLivenessQuiesce := supervisorLivenessQuiesce
	oldKill := darwinSupervisorKill

	t.Cleanup(func() {
		supervisorVerifyTrustedIdentity = oldVerify
		supervisorAcquireIdentityAuthority = oldAcquire
		supervisorAdoptIdentityLock = oldAdoptLock
		supervisorAdoptAuthorityDomain = oldAdoptDomain
		supervisorValidateAdoptedAuthority = oldValidateAuthority
		supervisorMarkerRoot = oldMarkerRoot
		supervisorPipe = oldPipe
		supervisorOpenIdentityPlaceholder = oldPlaceholder
		supervisorInheritedFile = oldInherited
		supervisorWriteConfig = oldWriteConfig
		supervisorValidateGuardianPeer = oldValidatePeer
		supervisorGuardianPeer = oldPeer
		supervisorQuarantineRetry = oldQuarantineRetry
		supervisorGuardianQuarantineRetry = oldGuardianRetry
		supervisorLivenessQuiesce = oldLivenessQuiesce
		darwinSupervisorKill = oldKill
	})
}

// unwritableMarkerPath returns a proof path whose parent denies creation while
// still permitting the stat lookups the proof loops perform.
func unwritableMarkerPath(t *testing.T, name string) string {
	t.Helper()

	sealed := filepath.Join(t.TempDir(), "sealed")
	require.NoError(t, os.Mkdir(sealed, 0o500))
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o700) })

	return filepath.Join(sealed, name)
}

func TestWriteSupervisorConfigDescriptorFailures(t *testing.T) {
	t.Run("secure", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorCreateTemp = func(dir string, pattern string) (*os.File, error) {
			file, err := os.CreateTemp(dir, pattern)
			require.NoError(t, err)
			require.NoError(t, file.Close())

			return file, nil
		}
		_, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.ErrorContains(t, err, "secure private supervisor config")
	})

	t.Run("rewind", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorCreateTemp = func(dir string, _ string) (*os.File, error) {
			path := filepath.Join(dir, "supervisor-config-fifo")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				return nil, err
			}

			return os.OpenFile(path, os.O_RDWR, 0)
		}
		_, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.ErrorContains(t, err, "rewind private supervisor config")
	})

	t.Run("unlink", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorCreateTemp = func(dir string, pattern string) (*os.File, error) {
			file, err := os.CreateTemp(dir, pattern)
			require.NoError(t, err)
			require.NoError(t, os.Remove(file.Name()))

			return file, nil
		}
		_, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.ErrorContains(t, err, "unlink private supervisor config")
	})
}

func TestSupervisorProofNilReceiversAndQuarantineProofs(t *testing.T) {
	require.NoError(t, (*supervisorProof)(nil).closeInherited())
	(*supervisorProof)(nil).removeTerminalMarkers()

	quarantined, err := (*supervisorProof)(nil).quarantineDetected()
	require.NoError(t, err)
	require.False(t, quarantined)

	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	t.Run("startup quarantine", func(t *testing.T) {
		quarantine := filepath.Join(root, "startup-quarantine")
		require.NoError(t, writeSupervisorMarker(quarantine))
		err := (&supervisorProof{
			started:    filepath.Join(root, "startup-missing-start"),
			completion: filepath.Join(root, "startup-missing-complete"),
			quarantine: quarantine,
		}).awaitCompletion()
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, "retained the identity lock")
	})

	t.Run("startup quarantine stat", func(t *testing.T) {
		err := (&supervisorProof{
			started:    filepath.Join(root, "stat-missing-start"),
			completion: filepath.Join(root, "stat-missing-complete"),
			quarantine: filepath.Join(blocker, "child"),
		}).awaitCompletion()
		require.ErrorContains(t, err, "stat liveness quarantine proof")
	})

	t.Run("completion quarantine", func(t *testing.T) {
		started := filepath.Join(root, "late-started")
		quarantine := filepath.Join(root, "late-quarantine")
		require.NoError(t, writeSupervisorMarker(started))
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = writeSupervisorMarker(quarantine)
		}()
		err := (&supervisorProof{
			started:    started,
			completion: filepath.Join(root, "late-missing-complete"),
			quarantine: quarantine,
		}).awaitCompletion()
		require.ErrorContains(t, err, "retained the identity lock")
	})

	t.Run("completion quarantine stat", func(t *testing.T) {
		started := filepath.Join(root, "stat-started")
		parent := filepath.Join(root, "stat-parent")
		require.NoError(t, writeSupervisorMarker(started))
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = os.WriteFile(parent, []byte("x"), 0o600)
		}()
		err := (&supervisorProof{
			started:    started,
			completion: filepath.Join(root, "stat-missing-complete"),
			quarantine: filepath.Join(parent, "child"),
		}).awaitCompletion()
		require.ErrorContains(t, err, "stat liveness quarantine proof")
	})
}

func TestSupervisorAwaitCommandObservesQuarantine(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	waitDone := make(chan error)

	quarantine := filepath.Join(root, "quarantine")
	require.NoError(t, writeSupervisorMarker(quarantine))
	waitErr, proofErr := (&supervisorProof{
		started:    filepath.Join(root, "missing-start"),
		completion: filepath.Join(root, "missing-complete"),
		quarantine: quarantine,
	}).awaitCommand(waitDone)
	require.NoError(t, waitErr)
	require.ErrorContains(t, proofErr, "retained the identity lock")

	waitErr, proofErr = (&supervisorProof{quarantine: filepath.Join(blocker, "child")}).awaitCommand(waitDone)
	require.NoError(t, waitErr)
	require.ErrorContains(t, proofErr, "stat liveness quarantine proof")
}

func TestRunSupervisorIdentityDispatch(t *testing.T) {
	newConfigFile := func(t *testing.T, config supervisorConfig) *os.File {
		t.Helper()

		file, err := writeSupervisorConfig(t.TempDir(), config)
		require.NoError(t, err)
		t.Cleanup(func() { _ = file.Close() })

		return file
	}

	baseConfig := func(t *testing.T) supervisorConfig {
		t.Helper()
		root := t.TempDir()

		return supervisorConfig{
			NativePath: "/bin/sleep", NativeArgs: []string{"0.1"}, NativeEnv: os.Environ(),
			Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
			LifecycleKind: lifecycleRuntime, DarwinBestEffort: true,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
			NativePIDFile: filepath.Join(root, "pid"),
			IsolationUID:  1, IsolationGID: 2,
		}
	}

	t.Run("unreadable config", func(t *testing.T) {
		require.ErrorContains(t, runSupervisor(supervisorModeLiveness, nil), "missing private supervisor config")
	})

	t.Run("inconsistent identity", func(t *testing.T) {
		config := baseConfig(t)
		config.IdentityLock = true
		require.ErrorContains(t, runSupervisor(supervisorModeLiveness, newConfigFile(t, config)),
			"UID lock and authority domain are inconsistent")
	})

	t.Run("adopt lock failure", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) {
			return nil, errors.New("adopt lock failed")
		}
		config := baseConfig(t)
		config.IdentityLock = true
		config.AuthorityDomain = true
		require.ErrorContains(t, runSupervisor(supervisorModeLiveness, newConfigFile(t, config)), "adopt lock failed")
	})

	t.Run("adopt domain failure", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) {
			return nil, errors.New("adopt domain failed")
		}
		config := baseConfig(t)
		config.IdentityLock = true
		config.AuthorityDomain = true
		require.ErrorContains(t, runSupervisor(supervisorModeLiveness, newConfigFile(t, config)), "adopt domain failed")
	})

	t.Run("adopted authority validation failure", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorValidateAdoptedAuthority = func(supervisorConfig) error {
			return errors.New("adopted authority invalid")
		}

		config := baseConfig(t)
		config.IdentityLock = true
		config.AuthorityDomain = true
		require.ErrorContains(t, runSupervisor(supervisorModeLiveness, newConfigFile(t, config)), "adopted authority invalid")
	})

	t.Run("adopted liveness releases both capabilities", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard

		closed := 0
		supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) {
			return countingIdentityLock{closed: &closed}, nil
		}
		supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) {
			return countingIdentityLock{closed: &closed}, nil
		}
		supervisorOpenLivenessContainment = func(supervisorConfig) (*livenessContainment, error) {
			return nil, errors.New("containment refused")
		}

		config := baseConfig(t)
		config.IdentityLock = true
		config.AuthorityDomain = true

		require.ErrorContains(t, runSupervisor(supervisorModeLiveness, newConfigFile(t, config)), "containment refused")
		require.Equal(t, 2, closed)
	})

	t.Run("guardian dispatch", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		supervisorExecutable = func() (string, error) { return "", errors.New("no liveness executable") }

		require.ErrorContains(t, runSupervisor(supervisorModeGuardian, newConfigFile(t, baseConfig(t))),
			"resolve liveness supervisor executable")
	})
}

// guardianGapConfig is one isolated guardian run: a private home lock, a
// private scratch root, and the proof paths that run owns.
func guardianGapConfig(t *testing.T) supervisorConfig {
	t.Helper()
	root := t.TempDir()
	isolation := testProcessIsolation()

	return supervisorConfig{
		NativePath: "/usr/bin/true", NativeEnv: os.Environ(),
		Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
		LifecycleKind: lifecycleRuntime, DarwinBestEffort: true,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		NativePIDFile: filepath.Join(root, "pid"),
		IsolationUID:  isolation.UID, IsolationGID: isolation.GID,
	}
}

func TestRunGuardianIdentityAcquisition(t *testing.T) {
	for name, test := range map[string]struct {
		identityLock bool
		arrange      func(*testing.T)
		wantError    string
	}{
		"trusted identity refused": {
			arrange: func(*testing.T) {
				supervisorVerifyTrustedIdentity = func(uint32) error { return errors.New("untrusted identity") }
			},
			wantError: "untrusted identity",
		},
		"standalone acquisition refused": {
			arrange: func(*testing.T) {
				supervisorAcquireIdentityAuthority = func(
					uint32, uint32, string, string, io.Reader,
				) (supervisorIdentityLock, supervisorIdentityLock, error) {
					return nil, nil, errors.New("acquire failed")
				}
			},
			wantError: "acquire failed",
		},
		"standalone acquisition": {
			arrange:   func(*testing.T) {},
			wantError: "resolve liveness supervisor executable",
		},
		"adopted lock refused": {
			identityLock: true,
			arrange: func(*testing.T) {
				supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) {
					return nil, errors.New("adopt lock failed")
				}
			},
			wantError: "adopt lock failed",
		},
		"adopted domain refused": {
			identityLock: true,
			arrange: func(*testing.T) {
				supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) {
					return nil, errors.New("adopt domain failed")
				}
			},
			wantError: "adopt domain failed",
		},
		"adopted authority refused": {
			identityLock: true,
			arrange: func(*testing.T) {
				supervisorValidateAdoptedAuthority = func(supervisorConfig) error {
					return errors.New("adopted authority invalid")
				}
			},
			wantError: "adopted authority invalid",
		},
		"adopted acquisition": {
			identityLock: true,
			arrange:      func(*testing.T) {},
			wantError:    "resolve liveness supervisor executable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			preserveSupervisorGlobals(t)
			preserveSupervisorIdentityGlobals(t)
			supervisorInput = strings.NewReader("")
			supervisorOutput = io.Discard
			supervisorError = io.Discard
			supervisorExecutable = func() (string, error) { return "", errors.New("no liveness executable") }
			test.arrange(t)

			config := guardianGapConfig(t)
			config.IdentityLock = test.identityLock
			config.AuthorityDomain = test.identityLock

			require.ErrorContains(t, runGuardian(config), test.wantError)
		})
	}
}

func TestRunGuardianPeerAndPlaceholderFailures(t *testing.T) {
	t.Run("guardian peer pipe", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorPipe = func() (*os.File, *os.File, error) { return nil, nil, errors.New("pipe exhausted") }
		require.ErrorContains(t, runGuardian(guardianGapConfig(t)), "open liveness guardian peer")
	})

	for name, failAt := range map[string]int{
		"identity placeholder":  1,
		"authority placeholder": 2,
	} {
		t.Run(name, func(t *testing.T) {
			preserveSupervisorGlobals(t)
			preserveSupervisorIdentityGlobals(t)

			opens := 0
			supervisorOpenIdentityPlaceholder = func() (*os.File, error) {
				opens++
				if opens == failAt {
					return nil, errors.New("placeholder unavailable")
				}

				return os.Open(os.DevNull)
			}
			require.ErrorContains(t, runGuardian(guardianGapConfig(t)), "open liveness "+strings.Fields(name)[0]+" placeholder")
		})
	}

	t.Run("liveness start", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		supervisorExecutable = func() (string, error) { return "/usr/bin/true", nil }
		missing := filepath.Join(t.TempDir(), "missing")
		supervisorExecCommand = func(string, ...string) *exec.Cmd { return exec.Command(missing) }

		require.ErrorContains(t, runGuardian(guardianGapConfig(t)), "start liveness supervisor")
	})
}

func TestRunGuardianQuarantineAndMarkerFailures(t *testing.T) {
	readyLine := supervisorReadyPrefix + `{"nativePid":123}`

	t.Run("pre-readiness quarantine", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		supervisorExecutable = func() (string, error) { return "/bin/sh", nil }
		supervisorExecCommand = func(string, ...string) *exec.Cmd {
			return exec.Command("/bin/sh", "-c", "exec 2>&-; sleep 0.3")
		}
		// The guardian's terminate step must not race the quarantine proof it
		// is supposed to observe, so the signal is neutralised for this run.
		darwinSupervisorKill = func(int, syscall.Signal) error { return nil }

		config := guardianGapConfig(t)
		config.Quarantine = filepath.Join(config.Scratch, "quarantine")
		require.NoError(t, writeSupervisorMarker(config.Quarantine))

		err := runGuardian(config)
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, "completed containment quarantine")
	})

	t.Run("pre-readiness quarantine marker", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		supervisorExecutable = func() (string, error) { return "/usr/bin/false", nil }

		config := guardianGapConfig(t)
		config.Quarantine = unwritableMarkerPath(t, "quarantine")
		require.NoError(t, writeSupervisorMarker(config.Started))

		require.ErrorContains(t, runGuardian(config), "write private supervisor proof")
	})

	t.Run("post-readiness quarantine", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		supervisorExecutable = func() (string, error) { return "/bin/sh", nil }
		supervisorExecCommand = func(string, ...string) *exec.Cmd {
			return exec.Command("/bin/sh", "-c", "printf '%s\\n' '"+readyLine+"' >&2; sleep 0.3")
		}

		config := guardianGapConfig(t)
		config.Quarantine = filepath.Join(config.Scratch, "quarantine")
		require.NoError(t, writeSupervisorMarker(config.Quarantine))

		err := runGuardian(config)
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, "completed containment quarantine")
	})

	t.Run("post-readiness quarantine marker", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		supervisorExecutable = func() (string, error) { return "/bin/sh", nil }
		supervisorExecCommand = func(string, ...string) *exec.Cmd {
			return exec.Command("/bin/sh", "-c", "printf '%s\\n' '"+readyLine+"' >&2; exit 0")
		}

		config := guardianGapConfig(t)
		config.Quarantine = unwritableMarkerPath(t, "quarantine")

		require.ErrorContains(t, runGuardian(config), "write private supervisor proof")
	})

	t.Run("framed guardian input", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorInput = strings.NewReader("payload")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		supervisorExecutable = func() (string, error) { return "/bin/sh", nil }
		supervisorExecCommand = func(string, ...string) *exec.Cmd {
			return exec.Command("/bin/sh", "-c", "printf '%s\\n' '"+readyLine+"' >&2; sleep 0.2")
		}

		config := guardianGapConfig(t)
		config.FramedInput = true
		require.NoError(t, writeSupervisorMarker(config.Completion))

		require.NoError(t, runGuardian(config))
	})
}

// countingIdentityLock records how many adopted capabilities were released.
type countingIdentityLock struct{ closed *int }

func (lock countingIdentityLock) Close() error {
	*lock.closed++

	return nil
}

func (countingIdentityLock) InheritedFile() *os.File { return nil }

// gapIdentityCapability is a duplicable process identity capability whose
// duplication outcome the test selects.
type gapIdentityCapability struct{ err error }

func (capability gapIdentityCapability) Duplicate() (*os.File, error) {
	if capability.err != nil {
		return nil, capability.err
	}

	return os.Open(os.DevNull)
}

func TestSupervisorCommandGuardBranches(t *testing.T) {
	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := supervisorCommand(ctx, supervisorConfig{Scratch: t.TempDir(), Isolation: testProcessIsolation()})
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("partial identity capability", func(t *testing.T) {
		isolation := testProcessIsolation()
		isolation.IdentityLock = gapIdentityCapability{}
		_, _, err := supervisorCommand(context.Background(), supervisorConfig{Scratch: t.TempDir(), Isolation: isolation})
		require.ErrorContains(t, err, "requires the UID lock and authority domain together")
	})

	t.Run("untrusted identity", func(t *testing.T) {
		preserveSupervisorIdentityGlobals(t)
		supervisorVerifyTrustedIdentity = func(uint32) error { return errors.New("untrusted identity") }
		_, _, err := supervisorCommand(context.Background(), supervisorConfig{Scratch: t.TempDir(), Isolation: testProcessIsolation()})
		require.ErrorContains(t, err, "untrusted identity")
	})

	t.Run("marker root", func(t *testing.T) {
		preserveSupervisorIdentityGlobals(t)
		supervisorMarkerRoot = func(supervisorConfig) (string, error) { return "", errors.New("no marker root") }
		_, _, err := supervisorCommand(context.Background(), supervisorConfig{Scratch: t.TempDir(), Isolation: testProcessIsolation()})
		require.ErrorContains(t, err, "no marker root")
	})

	t.Run("executable policy", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorExecutable = func() (string, error) { return "acp-go-codex", nil }
		_, _, err := supervisorCommand(context.Background(), supervisorConfig{Scratch: t.TempDir(), Isolation: testProcessIsolation()})
		require.ErrorContains(t, err, "resolve embedded runtime supervisor through process policy")
	})

	for name, isolate := range map[string]func(*ProcessIsolation){
		"identity lock duplicate": func(isolation *ProcessIsolation) {
			isolation.IdentityLock = gapIdentityCapability{err: errors.New("lock duplicate failed")}
			isolation.AuthorityDomain = gapIdentityCapability{}
		},
		"authority domain duplicate": func(isolation *ProcessIsolation) {
			isolation.IdentityLock = gapIdentityCapability{}
			isolation.AuthorityDomain = gapIdentityCapability{err: errors.New("domain duplicate failed")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			isolation := testProcessIsolation()
			isolate(isolation)
			_, _, err := supervisorCommand(context.Background(), supervisorConfig{
				NativePath: "/usr/bin/true", Home: filepath.Join(t.TempDir(), "home"), Scratch: t.TempDir(),
				Isolation: isolation,
			})
			require.ErrorContains(t, err, "duplicate failed")
		})
	}

	t.Run("inherits identity capabilities", func(t *testing.T) {
		isolation := testProcessIsolation()
		isolation.IdentityLock = gapIdentityCapability{}
		isolation.AuthorityDomain = gapIdentityCapability{}

		cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
			NativePath: "/usr/bin/true", Home: filepath.Join(t.TempDir(), "home"), Scratch: t.TempDir(),
			Isolation: isolation,
		})
		require.NoError(t, err)
		require.Len(t, cmd.ExtraFiles, 3)
		require.Len(t, proof.inherited, 3)
		require.NoError(t, proof.closeInherited())
	})
}

func TestRunLivenessIdentityAndPeerFailures(t *testing.T) {
	livenessGapConfig := func(t *testing.T) supervisorConfig {
		t.Helper()
		root := t.TempDir()

		return supervisorConfig{
			NativePath: "/usr/bin/true", NativeEnv: os.Environ(),
			Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
			LifecycleKind: lifecycleRuntime, DarwinBestEffort: true,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
			NativePIDFile: filepath.Join(root, "pid"),
		}
	}

	t.Run("untrusted identity", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorVerifyTrustedIdentity = func(uint32) error { return errors.New("untrusted identity") }

		config := livenessGapConfig(t)
		config.IsolationUID = 1
		config.IsolationGID = 2
		require.ErrorContains(t, runLiveness(config), "untrusted identity")
	})

	t.Run("credential refused", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard

		config := livenessGapConfig(t)
		config.IsolationUID = 1
		require.ErrorContains(t, runLiveness(config), "apply supervised Codex native identity")
	})

	t.Run("guardian peer refused", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		supervisorValidateGuardianPeer = func(*os.File, <-chan struct{}) error { return errors.New("peer lost") }

		require.ErrorContains(t, runLiveness(livenessGapConfig(t)), "peer lost")
	})

	t.Run("guardian peer refused at start", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard

		checks := 0
		supervisorValidateGuardianPeer = func(*os.File, <-chan struct{}) error {
			checks++
			if checks == 1 {
				return nil
			}

			return errors.New("peer lost before start")
		}

		require.ErrorContains(t, runLiveness(livenessGapConfig(t)), "peer lost before start")
	})

	t.Run("guardian peer stream", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard

		peerRead, peerWrite, err := os.Pipe()
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = peerWrite.Close()
			_ = peerRead.Close()
		})
		supervisorGuardianPeer = peerRead

		require.NoError(t, runLiveness(livenessGapConfig(t)))
	})
}

func TestCompleteOrQuarantineRetryPaths(t *testing.T) {
	arrangeStreams := func(t *testing.T) {
		t.Helper()

		closable, err := os.CreateTemp(t.TempDir(), "quarantine-stream")
		require.NoError(t, err)
		supervisorInput = closable
		supervisorOutput = io.Discard
		supervisorError = io.Discard
	}

	quarantineConfig := func(t *testing.T, quarantine string) supervisorConfig {
		t.Helper()
		root := t.TempDir()
		config := supervisorConfig{
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
			Quarantine: quarantine, NativePIDFile: filepath.Join(root, "pid"),
			ProviderSnapshot: filepath.Join(root, "snapshot"),
		}
		require.NoError(t, writeSupervisorMarker(config.Started))
		require.NoError(t, writeSupervisorMarker(config.NativePIDFile))
		require.NoError(t, writeSupervisorMarker(config.ProviderSnapshot))

		return config
	}

	t.Run("liveness retry", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		arrangeStreams(t)
		supervisorQuarantineRetry = func(*livenessContainment) error { return errors.New("liveness retry failed") }

		config := quarantineConfig(t, filepath.Join(t.TempDir(), "quarantine"))
		err := completeOrQuarantineLiveness(config, nil, errors.New("unproven containment"))
		require.ErrorContains(t, err, "unproven containment")
		require.ErrorContains(t, err, "liveness retry failed")
		require.FileExists(t, config.Quarantine)
		require.NoFileExists(t, config.Started)
		require.NoFileExists(t, config.ProviderSnapshot)
	})

	t.Run("liveness quarantine marker", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorQuarantineRetry = func(*livenessContainment) error { return nil }

		config := quarantineConfig(t, unwritableMarkerPath(t, "quarantine"))
		err := completeOrQuarantineLiveness(config, nil, errors.New("unproven containment"))
		require.ErrorContains(t, err, "unproven containment")
		require.ErrorContains(t, err, "write private supervisor proof")
	})

	t.Run("guardian retry", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		arrangeStreams(t)
		supervisorGuardianQuarantineRetry = func(*guardianContainment) error { return errors.New("guardian retry failed") }

		config := quarantineConfig(t, filepath.Join(t.TempDir(), "quarantine"))
		err := completeOrQuarantineGuardian(config, nil, errors.New("unproven containment"))
		require.ErrorContains(t, err, "unproven containment")
		require.ErrorContains(t, err, "guardian retry failed")
		require.NoFileExists(t, config.Quarantine)
		require.NoFileExists(t, config.Started)
	})

	t.Run("guardian quarantine marker", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		supervisorGuardianQuarantineRetry = func(*guardianContainment) error { return nil }

		config := quarantineConfig(t, unwritableMarkerPath(t, "quarantine"))
		err := completeOrQuarantineGuardian(config, nil, errors.New("unproven containment"))
		require.ErrorContains(t, err, "unproven containment")
		require.ErrorContains(t, err, "write private supervisor proof")
	})
}

func TestSupervisorBootstrapInheritedDescriptors(t *testing.T) {
	t.Run("missing config descriptor", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		t.Setenv(supervisorModeEnv, supervisorModeGuardian)

		var reported strings.Builder

		supervisorError = &reported

		exitCode := 0
		supervisorExit = func(code int) { exitCode = code }
		supervisorInheritedFile = func(uintptr, string) *os.File { return nil }

		supervisorBootstrap()
		require.Equal(t, 1, exitCode)
		require.Contains(t, reported.String(), "inherited config descriptor is unavailable")
	})

	t.Run("missing guardian peer descriptor", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		t.Setenv(supervisorModeEnv, supervisorModeLiveness)

		configFile, err := os.CreateTemp(t.TempDir(), "config")
		require.NoError(t, err)

		var reported strings.Builder

		supervisorError = &reported

		exitCode := 0
		supervisorExit = func(code int) { exitCode = code }
		supervisorInheritedFile = func(fd uintptr, _ string) *os.File {
			if fd == 3 {
				return configFile
			}

			return nil
		}

		supervisorBootstrap()
		require.Equal(t, 1, exitCode)
		require.Contains(t, reported.String(), "guardian peer descriptor is unavailable")
	})

	t.Run("liveness inherits both descriptors", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		t.Setenv(supervisorModeEnv, supervisorModeLiveness)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		nativeEnv := make([]string, 0, len(os.Environ()))
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, supervisorModeEnv+"=") {
				nativeEnv = append(nativeEnv, entry)
			}
		}

		root := t.TempDir()
		configFile, err := writeSupervisorConfig(root, supervisorConfig{
			NativePath: filepath.Join(root, "missing-native"), NativeEnv: nativeEnv,
			Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
			LifecycleKind: lifecycleRuntime, DarwinBestEffort: true,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
			NativePIDFile: filepath.Join(root, "pid"),
			IsolationUID:  1, IsolationGID: 2,
		})
		require.NoError(t, err)

		peerRead, peerWrite, pipeErr := os.Pipe()
		require.NoError(t, pipeErr)
		t.Cleanup(func() {
			_ = peerWrite.Close()
			_ = peerRead.Close()
		})

		var reported strings.Builder

		supervisorError = &reported

		exitCode := 0
		supervisorExit = func(code int) { exitCode = code }
		supervisorInheritedFile = func(fd uintptr, _ string) *os.File {
			if fd == 3 {
				return configFile
			}

			return peerRead
		}

		supervisorBootstrap()
		require.Equal(t, 1, exitCode)
		require.Contains(t, reported.String(), "start contained native root")
	})

	t.Run("guardian completes", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveSupervisorIdentityGlobals(t)
		t.Setenv(supervisorModeEnv, supervisorModeGuardian)
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard

		root := t.TempDir()
		completion := filepath.Join(root, "complete")
		require.NoError(t, writeSupervisorMarker(completion))
		configFile, err := writeSupervisorConfig(root, supervisorConfig{
			NativePath: "/usr/bin/true", NativeEnv: os.Environ(),
			Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
			LifecycleKind: lifecycleRuntime, DarwinBestEffort: true,
			Started: filepath.Join(root, "started"), Completion: completion,
			NativePIDFile: filepath.Join(root, "pid"),
			IsolationUID:  1, IsolationGID: 2,
		})
		require.NoError(t, err)

		supervisorExecutable = func() (string, error) { return "/bin/sh", nil }
		supervisorExecCommand = func(string, ...string) *exec.Cmd {
			return exec.Command("/bin/sh", "-c",
				"printf '%s\\n' '"+supervisorReadyPrefix+`{"nativePid":123}`+"' >&2; exit 0")
		}

		exitCode := -1
		supervisorExit = func(code int) { exitCode = code }
		supervisorInheritedFile = func(uintptr, string) *os.File { return configFile }

		supervisorBootstrap()
		require.Equal(t, 0, exitCode)
	})
}
