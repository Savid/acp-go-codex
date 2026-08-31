package codex

import (
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

// swapSupervisorSeam captures a package seam, registers its restoration, and
// installs the replacement in one statement block so no case can leak a stub
// into a sibling test.
func swapSupervisorSeam[T any](t *testing.T, seam *T, replacement T) {
	t.Helper()

	previous := *seam
	t.Cleanup(func() { *seam = previous })
	*seam = replacement
}

// gapSupervisorIdentityLock stands in for an adopted capability. The guardian
// only ever asks an adopted lock for its inherited descriptor, and an adopted
// lock that carries none drives the placeholder path instead.
type gapSupervisorIdentityLock struct{}

func (gapSupervisorIdentityLock) Close() error            { return nil }
func (gapSupervisorIdentityLock) InheritedFile() *os.File { return nil }

// gapHeldOutput holds the guardian's stdout forwarder inside its first write.
// exec.Cmd.Wait does not return until that forwarder finishes, so holding it
// keeps the liveness wait result pending and makes awaitLivenessTerminal reach
// its ticker arm instead of racing a fast child's exit.
type gapHeldOutput struct {
	hold time.Duration
	once sync.Once
}

func (writer *gapHeldOutput) Write(value []byte) (int, error) {
	writer.once.Do(func() { time.Sleep(writer.hold) })

	return len(value), nil
}

// gapGuardianConfig names a private supervisor run whose proof paths all live
// under one fresh scratch root.
func gapGuardianConfig(t *testing.T) supervisorConfig {
	t.Helper()

	root := t.TempDir()

	return supervisorConfig{
		NativePath:       "/usr/bin/true",
		NativeEnv:        os.Environ(),
		Home:             filepath.Join(root, "home"),
		Scratch:          root,
		ScratchParent:    filepath.Dir(root),
		LifecycleKind:    lifecycleRuntime,
		DarwinBestEffort: true,
		Started:          filepath.Join(root, "started"),
		Completion:       filepath.Join(root, "complete"),
		NativePIDFile:    filepath.Join(root, "pid"),
	}
}

// gapUncreatableMarker names a marker whose parent directory does not exist.
// os.Stat reports it absent, so a quarantine poll reads "not yet quarantined",
// while O_CREAT still fails for every identity including root. A mode-stripped
// directory would not hold that second property under the privileged suite.
func gapUncreatableMarker(t *testing.T, name string) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "absent-directory", name)
}

// gapUnstatableMarker names a marker below a regular file. Every stat of it
// fails with a non-ENOENT error, which is what the fail-closed proof polls
// treat as a terminal answer.
func gapUnstatableMarker(t *testing.T, name string) string {
	t.Helper()

	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	return filepath.Join(blocker, name)
}

// installGapStreams points the three supervisor streams at inert test values.
func installGapStreams(t *testing.T, output io.Writer) {
	t.Helper()

	swapSupervisorSeam(t, &supervisorInput, io.Reader(strings.NewReader("")))
	swapSupervisorSeam(t, &supervisorOutput, output)
	swapSupervisorSeam(t, &supervisorError, io.Discard)
}

// installGapNeutralIdentity silences the platform identity seams so a case
// exercises only the arm it names rather than the host's real agent registry.
func installGapNeutralIdentity(t *testing.T) {
	t.Helper()

	swapSupervisorSeam(t, &supervisorVerifyTrustedIdentity, func(uint32) error { return nil })
	swapSupervisorSeam(t, &supervisorAdoptIdentityLock, func(uint32) (supervisorIdentityLock, error) {
		return gapSupervisorIdentityLock{}, nil
	})
	swapSupervisorSeam(t, &supervisorAdoptAuthorityDomain, func(uint32) (supervisorIdentityLock, error) {
		return gapSupervisorIdentityLock{}, nil
	})
	swapSupervisorSeam(t, &supervisorValidateAdoptedAuthority, func(supervisorConfig) error { return nil })
	swapSupervisorSeam(t, &supervisorAcquireIdentityAuthority, func(
		uint32, uint32, string, string, io.Reader,
	) (supervisorIdentityLock, supervisorIdentityLock, error) {
		return gapSupervisorIdentityLock{}, gapSupervisorIdentityLock{}, nil
	})
	swapSupervisorSeam(t, &supervisorGuardianPeer, (*os.File)(nil))
	swapSupervisorSeam(t, &supervisorValidateGuardianPeer, func(*os.File, <-chan struct{}) error { return nil })
}

// installGapGuardianLaunch replaces the pieces of the guardian's launch that
// differ per platform with portable equivalents, so the arms under test are the
// only variable: a containment handle with no host resources, a private config
// descriptor backed by a plain file, and a scripted liveness child.
func installGapGuardianLaunch(t *testing.T, script string) {
	t.Helper()

	swapSupervisorSeam(t, &supervisorNewGuardianContainment, func(supervisorConfig) (*guardianContainment, error) {
		return &guardianContainment{}, nil
	})
	swapSupervisorSeam(t, &supervisorWriteConfig, func(string, supervisorConfig) (*os.File, error) {
		return os.CreateTemp(t.TempDir(), "liveness-config")
	})
	swapSupervisorSeam(t, &supervisorExecutable, func() (string, error) { return "/bin/sh", nil })
	swapSupervisorSeam(t, &supervisorExecCommand, func(string, ...string) *exec.Cmd {
		return exec.Command("/bin/sh", "-c", script)
	})
}

const gapReadyLine = supervisorReadyPrefix + `{"nativePid":4242}`

// The guardian refuses to continue the moment any step of establishing the
// isolated identity fails, and it returns that failure verbatim rather than a
// wrapped or substituted one. Each case fails exactly one step and proves the
// guardian surfaced it instead of proceeding to the home claim.
func TestRunGuardianIdentityAuthorityArms(t *testing.T) {
	t.Run("trusted identity refused", func(t *testing.T) {
		installGapNeutralIdentity(t)

		want := errors.New("untrusted root identity")
		swapSupervisorSeam(t, &supervisorVerifyTrustedIdentity, func(uid uint32) error {
			require.Equal(t, uint32(7), uid)

			return want
		})

		require.ErrorIs(t, runGuardian(supervisorConfig{IsolationUID: 7}), want)
	})

	t.Run("adopted identity lock refused", func(t *testing.T) {
		installGapNeutralIdentity(t)

		want := errors.New("identity lock unavailable")
		swapSupervisorSeam(t, &supervisorAdoptIdentityLock, func(uint32) (supervisorIdentityLock, error) {
			return nil, want
		})
		swapSupervisorSeam(t, &supervisorAdoptAuthorityDomain, func(uint32) (supervisorIdentityLock, error) {
			require.Fail(t, "authority domain adopted after the identity lock was refused")

			return nil, errors.New("unreachable")
		})

		require.ErrorIs(t, runGuardian(supervisorConfig{IsolationUID: 7, IdentityLock: true}), want)
	})

	t.Run("adopted authority domain refused", func(t *testing.T) {
		installGapNeutralIdentity(t)

		want := errors.New("authority domain unavailable")
		swapSupervisorSeam(t, &supervisorAdoptAuthorityDomain, func(uint32) (supervisorIdentityLock, error) {
			return nil, want
		})
		swapSupervisorSeam(t, &supervisorValidateAdoptedAuthority, func(supervisorConfig) error {
			require.Fail(t, "adopted authority validated after the domain was refused")

			return nil
		})

		require.ErrorIs(t, runGuardian(supervisorConfig{IsolationUID: 7, IdentityLock: true}), want)
	})

	t.Run("adopted authority invalid", func(t *testing.T) {
		installGapNeutralIdentity(t)

		want := errors.New("adopted authority does not match the config")
		validated := 0
		swapSupervisorSeam(t, &supervisorValidateAdoptedAuthority, func(config supervisorConfig) error {
			validated++
			require.Equal(t, uint32(7), config.IsolationUID)

			return want
		})

		require.ErrorIs(t, runGuardian(supervisorConfig{IsolationUID: 7, IdentityLock: true}), want)
		require.Equal(t, 1, validated)
	})

	t.Run("identity authority acquisition refused", func(t *testing.T) {
		installGapNeutralIdentity(t)

		want := errors.New("identity authority contended")
		swapSupervisorSeam(t, &supervisorAcquireIdentityAuthority, func(
			uid uint32, gid uint32, owner string, stateRoot string, _ io.Reader,
		) (supervisorIdentityLock, supervisorIdentityLock, error) {
			require.Equal(t, uint32(7), uid)
			require.Equal(t, uint32(9), gid)
			require.Equal(t, "owner", owner)
			require.Equal(t, "/state", stateRoot)

			return nil, nil, want
		})

		err := runGuardian(supervisorConfig{
			IsolationUID: 7, IsolationGID: 9,
			StandaloneOwnerID: "owner", StandaloneStateRoot: "/state",
		})
		require.ErrorIs(t, err, want)
	})
}

// Before it launches the liveness supervisor the guardian must open the peer
// pipe, two inherited identity placeholders, and the private config
// descriptor. Each failure aborts the launch under its own message so an
// operator can tell which descriptor the kernel refused.
func TestRunGuardianDescriptorSetupArms(t *testing.T) {
	t.Run("guardian peer pipe refused", func(t *testing.T) {
		installGapNeutralIdentity(t)
		installGapStreams(t, io.Discard)
		installGapGuardianLaunch(t, "exit 0")

		want := errors.New("pipe exhausted")
		swapSupervisorSeam(t, &supervisorPipe, func() (*os.File, *os.File, error) { return nil, nil, want })

		err := runGuardian(gapGuardianConfig(t))
		require.ErrorIs(t, err, want)
		require.ErrorContains(t, err, "open liveness guardian peer")
	})

	for name, failAt := range map[string]int{"identity": 1, "authority": 2} {
		t.Run(name+" placeholder refused", func(t *testing.T) {
			installGapNeutralIdentity(t)
			installGapStreams(t, io.Discard)
			installGapGuardianLaunch(t, "exit 0")

			want := errors.New("placeholder unavailable")
			opens := 0
			swapSupervisorSeam(t, &supervisorOpenIdentityPlaceholder, func() (*os.File, error) {
				opens++
				if opens == failAt {
					return nil, want
				}

				return os.Open(os.DevNull)
			})

			err := runGuardian(gapGuardianConfig(t))
			require.ErrorIs(t, err, want)
			require.ErrorContains(t, err, "open liveness "+name+" placeholder")
			require.Equal(t, failAt, opens)
		})
	}

	t.Run("private config descriptor refused", func(t *testing.T) {
		installGapNeutralIdentity(t)
		installGapStreams(t, io.Discard)
		installGapGuardianLaunch(t, "exit 0")

		want := errors.New("config descriptor unavailable")
		launched := false
		swapSupervisorSeam(t, &supervisorWriteConfig, func(string, supervisorConfig) (*os.File, error) {
			return nil, want
		})
		swapSupervisorSeam(t, &supervisorExecutable, func() (string, error) {
			launched = true

			return "/bin/sh", nil
		})

		require.ErrorIs(t, runGuardian(gapGuardianConfig(t)), want)
		require.False(t, launched, "guardian resolved an executable after the config descriptor was refused")
	})
}

// A liveness supervisor that quarantines itself, or a quarantine proof the
// guardian cannot even read, is terminal on both sides of readiness: the
// guardian stops forwarding, joins the terminal cause, and finishes the
// quarantine by reaping the child and clearing every proof marker.
func TestRunGuardianQuarantineTerminalArms(t *testing.T) {
	t.Run("pre-readiness quarantine proof observed", func(t *testing.T) {
		installGapNeutralIdentity(t)
		installGapStreams(t, &gapHeldOutput{hold: 300 * time.Millisecond})
		installGapGuardianLaunch(t, "printf x; exit 0")

		config := gapGuardianConfig(t)
		config.Quarantine = filepath.Join(config.Scratch, "quarantine")
		require.NoError(t, writeSupervisorMarker(config.Started))
		require.NoError(t, writeSupervisorMarker(config.Quarantine))

		err := runGuardian(config)
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, "completed containment quarantine")
		require.NoFileExists(t, config.Quarantine)
		require.NoFileExists(t, config.Started)
	})

	t.Run("post-readiness quarantine proof unreadable", func(t *testing.T) {
		installGapNeutralIdentity(t)
		installGapStreams(t, &gapHeldOutput{hold: 300 * time.Millisecond})
		installGapGuardianLaunch(t, "printf '%s\\n' '"+gapReadyLine+"' >&2; printf x; exit 0")

		config := gapGuardianConfig(t)
		config.Quarantine = gapUnstatableMarker(t, "quarantine")
		require.NoError(t, writeSupervisorMarker(config.Started))

		err := runGuardian(config)
		require.ErrorContains(t, err, "stat liveness quarantine proof")
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, "completed containment quarantine")
		require.NoFileExists(t, config.Started)
		require.NoFileExists(t, config.Completion)
	})
}

// When the liveness supervisor leaves no completion proof the guardian must
// publish a quarantine marker before it starts its own containment proof. A
// marker it cannot write is fatal on both sides of readiness: the guardian
// reports the write failure and never claims the run completed.
func TestRunGuardianQuarantineMarkerArms(t *testing.T) {
	t.Run("pre-readiness marker refused", func(t *testing.T) {
		installGapNeutralIdentity(t)
		installGapStreams(t, io.Discard)
		installGapGuardianLaunch(t, "exit 0")

		quiesced := false
		swapSupervisorSeam(t, &supervisorGuardianQuiesce, func(*guardianContainment, int, time.Duration) error {
			quiesced = true

			return nil
		})

		config := gapGuardianConfig(t)
		config.Quarantine = gapUncreatableMarker(t, "quarantine")
		require.NoError(t, writeSupervisorMarker(config.Started))
		require.NoError(t, writeNativePID(config.NativePIDFile, 4242))

		err := runGuardian(config)
		require.ErrorContains(t, err, "write private supervisor proof")
		require.False(t, quiesced, "guardian proved quiescence without a quarantine marker")
		require.NoFileExists(t, config.Completion)
	})

	t.Run("post-readiness marker refused", func(t *testing.T) {
		installGapNeutralIdentity(t)
		installGapStreams(t, io.Discard)
		installGapGuardianLaunch(t, "printf '%s\\n' '"+gapReadyLine+"' >&2; exit 0")

		quiesced := false
		swapSupervisorSeam(t, &supervisorGuardianQuiesce, func(*guardianContainment, int, time.Duration) error {
			quiesced = true

			return nil
		})

		config := gapGuardianConfig(t)
		config.Quarantine = gapUncreatableMarker(t, "quarantine")

		err := runGuardian(config)
		require.ErrorContains(t, err, "write private supervisor proof")
		require.False(t, quiesced, "guardian proved quiescence without a quarantine marker")
		require.NoFileExists(t, config.Completion)
	})
}

// A completion proof the guardian can neither read nor call absent is not a
// missing proof: the guardian reports the stat failure instead of silently
// treating the run as complete or starting a recovery it has no basis for.
func TestRunGuardianCompletionStatArm(t *testing.T) {
	installGapNeutralIdentity(t)
	installGapStreams(t, io.Discard)
	installGapGuardianLaunch(t, "printf '%s\\n' '"+gapReadyLine+"' >&2; exit 0")

	quiesced := false
	swapSupervisorSeam(t, &supervisorGuardianQuiesce, func(*guardianContainment, int, time.Duration) error {
		quiesced = true

		return nil
	})

	config := gapGuardianConfig(t)
	config.Completion = gapUnstatableMarker(t, "complete")

	err := runGuardian(config)
	require.ErrorContains(t, err, "stat liveness completion")
	require.False(t, quiesced, "guardian started a containment proof it could not justify")
}

// The liveness supervisor refuses to launch the native root unless the trusted
// identity, the supervised credential, and the guardian peer all hold. The
// peer arm additionally owes a containment proof before it returns, because by
// then it already holds the liveness claim.
func TestRunLivenessIdentityAndPeerArms(t *testing.T) {
	t.Run("trusted identity refused", func(t *testing.T) {
		installGapNeutralIdentity(t)

		want := errors.New("untrusted root identity")
		swapSupervisorSeam(t, &supervisorVerifyTrustedIdentity, func(uid uint32) error {
			require.Equal(t, uint32(7), uid)

			return want
		})

		config := gapGuardianConfig(t)
		config.IsolationUID = 7
		config.IsolationGID = 9

		require.ErrorIs(t, runLiveness(config), want)
		require.NoFileExists(t, config.Started, "liveness published a start proof for an untrusted identity")
	})

	t.Run("supervised credential refused", func(t *testing.T) {
		installGapNeutralIdentity(t)
		installGapStreams(t, io.Discard)
		swapSupervisorSeam(t, &supervisorOpenLivenessContainment, func(supervisorConfig) (*livenessContainment, error) {
			return &livenessContainment{}, nil
		})

		config := gapGuardianConfig(t)
		// A zero GID cannot name a supervised identity, so the credential the
		// native root would run under is refused before any process starts.
		config.IsolationUID = 7
		config.IsolationGID = 0

		require.ErrorContains(t, runLiveness(config), "apply supervised Codex native identity")
		require.NoFileExists(t, config.Completion)
	})

	t.Run("guardian peer refused", func(t *testing.T) {
		installGapNeutralIdentity(t)
		installGapStreams(t, io.Discard)
		swapSupervisorSeam(t, &supervisorOpenLivenessContainment, func(supervisorConfig) (*livenessContainment, error) {
			return &livenessContainment{}, nil
		})

		want := errors.New("guardian peer lost")
		swapSupervisorSeam(t, &supervisorValidateGuardianPeer, func(*os.File, <-chan struct{}) error { return want })

		probes := 0
		swapSupervisorSeam(t, &supervisorLivenessQuiesce, func(_ *livenessContainment, nativePID int, _ time.Duration) error {
			probes++
			// No native root was ever started, so the proof must not name one.
			require.Zero(t, nativePID)

			return nil
		})

		config := gapGuardianConfig(t)

		err := runLiveness(config)
		require.ErrorIs(t, err, want)
		require.Equal(t, 1, probes, "liveness skipped the containment proof it owes after a lost peer")
		require.FileExists(t, config.Completion)
	})
}

// The quarantine helpers are the last fail-closed step of both supervisors.
// They must report a marker they cannot write instead of running the retry,
// must publish a named guardian quarantine marker, must treat an existing
// marker as a terminal answer, and must close the inherited standard streams
// so a retried containment cannot keep speaking to the caller.
func TestSupervisorQuarantineHelperArms(t *testing.T) {
	t.Run("liveness quarantine marker refused", func(t *testing.T) {
		retries := 0
		swapSupervisorSeam(t, &supervisorQuarantineRetry, func(*livenessContainment) error {
			retries++

			return nil
		})

		proof := errors.New("containment unproven")
		err := completeOrQuarantineLiveness(supervisorConfig{Quarantine: gapUncreatableMarker(t, "quarantine")}, nil, proof)
		require.ErrorIs(t, err, proof)
		require.ErrorContains(t, err, "write private supervisor proof")
		require.Zero(t, retries, "liveness retried containment without publishing a quarantine marker")
	})

	t.Run("guardian quarantine marker refused", func(t *testing.T) {
		retries := 0
		swapSupervisorSeam(t, &supervisorGuardianQuarantineRetry, func(*guardianContainment) error {
			retries++

			return nil
		})

		proof := errors.New("containment unproven")
		err := completeOrQuarantineGuardian(supervisorConfig{Quarantine: gapUncreatableMarker(t, "quarantine")}, nil, proof)
		require.ErrorIs(t, err, proof)
		require.ErrorContains(t, err, "write private supervisor proof")
		require.Zero(t, retries, "guardian retried containment without publishing a quarantine marker")
	})

	t.Run("guardian quarantine marker published", func(t *testing.T) {
		quarantine := filepath.Join(t.TempDir(), "quarantine")
		require.NoError(t, writeGuardianQuarantineMarker(supervisorConfig{Quarantine: quarantine}))
		require.FileExists(t, quarantine)
	})

	t.Run("existing quarantine proof is terminal", func(t *testing.T) {
		quarantine := filepath.Join(t.TempDir(), "quarantine")
		require.NoError(t, writeSupervisorMarker(quarantine))

		// The wait channel never delivers, so a true result can only come from
		// the marker poll rather than from the child's exit.
		waitErr, quarantined, terminalErr := awaitLivenessTerminal(make(chan error), quarantine)
		require.NoError(t, waitErr)
		require.True(t, quarantined)
		require.NoError(t, terminalErr)
	})

	t.Run("inherited streams closed", func(t *testing.T) {
		root := t.TempDir()
		streams := make([]*os.File, 0, 3)

		for _, name := range []string{"caller-input", "caller-output", "caller-error"} {
			file, err := os.Create(filepath.Join(root, name))
			require.NoError(t, err)

			streams = append(streams, file)
		}

		swapSupervisorSeam(t, &supervisorInput, io.Reader(streams[0]))
		swapSupervisorSeam(t, &supervisorOutput, io.Writer(streams[1]))
		swapSupervisorSeam(t, &supervisorError, io.Writer(streams[2]))

		closeSupervisorQuarantineStreams()

		for _, stream := range streams {
			require.ErrorIs(t, stream.Close(), os.ErrClosed, "quarantine left %s open to the caller", stream.Name())
		}
	})
}
