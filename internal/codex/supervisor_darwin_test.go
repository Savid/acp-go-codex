//go:build darwin

package codex

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func preserveDarwinSupervisorSeams(t *testing.T) {
	t.Helper()
	oldGetpgid := darwinSupervisorGetpgid
	oldKill := darwinSupervisorKill
	oldNow := darwinSupervisorNow
	oldSleep := darwinSupervisorSleep
	oldFastExitWait := darwinFastExitWait
	oldAbortWait := darwinAbortWait
	oldIdentity := darwinProcessIdentityLookup
	t.Cleanup(func() {
		darwinSupervisorGetpgid = oldGetpgid
		darwinSupervisorKill = oldKill
		darwinSupervisorNow = oldNow
		darwinSupervisorSleep = oldSleep
		darwinFastExitWait = oldFastExitWait
		darwinAbortWait = oldAbortWait
		darwinProcessIdentityLookup = oldIdentity
	})
}

func darwinSupervisorTestConfig(t *testing.T) supervisorConfig {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-codex-runtime-test")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	return supervisorConfig{
		ScratchParent: parent, Scratch: root, LifecycleKind: "runtime", DarwinBestEffort: true,
	}
}

func TestDarwinContainmentSelectionAndStartFailures(t *testing.T) {
	preserveDarwinSupervisorSeams(t)
	if _, err := newGuardianContainment(supervisorConfig{}); err == nil {
		t.Fatal("guardian accepted missing opt-in")
	}
	signalCalls := 0
	darwinSupervisorKill = func(int, syscall.Signal) error {
		signalCalls++

		return nil
	}
	if err := (&guardianContainment{}).Quiesce(0, time.Second); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("guardian manufactured no-start proof: %v", err)
	}
	if err := (&guardianContainment{}).Quiesce(123, time.Second); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("guardian manufactured post-start proof: %v", err)
	}
	if signalCalls != 0 {
		t.Fatalf("guardian signalled a recycled numeric identity %d times", signalCalls)
	}
	if _, err := openLivenessContainment(supervisorConfig{}); err == nil {
		t.Fatal("liveness accepted missing opt-in")
	}
	if _, err := openLivenessContainment(supervisorConfig{DarwinBestEffort: true}); err == nil {
		t.Fatal("liveness accepted missing generation root")
	}
	parent := t.TempDir()
	if _, err := openLivenessContainment(supervisorConfig{
		DarwinBestEffort: true, ScratchParent: parent,
		Scratch: filepath.Join(filepath.Dir(parent), "acp-go-codex-runtime-outside"), LifecycleKind: "runtime",
	}); err == nil {
		t.Fatal("liveness accepted a generation outside its parent")
	}

	config := darwinSupervisorTestConfig(t)
	liveness, err := openLivenessContainment(config)
	if err != nil {
		t.Fatal(err)
	}
	if startErr := liveness.Start(exec.Command(filepath.Join(t.TempDir(), "missing"))); startErr == nil {
		t.Fatal("missing command started")
	}

	config = darwinSupervisorTestConfig(t)
	liveness, err = openLivenessContainment(config)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/usr/bin/true")
	credential := &syscall.Credential{Uid: 501, Gid: 502, Groups: []uint32{}}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	want := errors.New("stop before launch")
	liveness.beforeStart = func() error {
		if command.SysProcAttr == nil || command.SysProcAttr.Credential != credential || !command.SysProcAttr.Setpgid {
			t.Fatalf("Darwin liveness start discarded native credentials: %#v", command.SysProcAttr)
		}

		return want
	}
	if err = liveness.Start(command); !errors.Is(err, want) {
		t.Fatalf("credential-preserving start = %v", err)
	}
	if err := (&livenessContainment{generation: &DarwinGeneration{}}).Start(exec.Command("/usr/bin/true")); err == nil {
		t.Fatal("invalid generation prepared a command")
	}

	if err := (*livenessContainment)(nil).Start(exec.Command("/usr/bin/true")); err == nil {
		t.Fatal("nil containment started")
	}
	if err := (&livenessContainment{}).Quiesce(0, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := (&livenessContainment{}).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinGuardianFallbackNeverSignalsReportedNativeIdentity(t *testing.T) {
	preserveDarwinSupervisorSeams(t)
	preserveSupervisorGlobals(t)

	root := t.TempDir()
	liveness := filepath.Join(root, "failed-liveness")
	if err := os.WriteFile(liveness, []byte("#!/bin/sh\nprintf '%s\\n' '"+supervisorReadyPrefix+`{"nativePid":87654321}`+"' >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	supervisorExecutable = func() (string, error) { return liveness, nil }
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = io.Discard

	signalCalls := 0
	darwinSupervisorKill = func(int, syscall.Signal) error {
		signalCalls++

		return nil
	}
	err := runGuardian(supervisorConfig{
		Home: filepath.Join(root, "home"), Scratch: root, DarwinBestEffort: true,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		NativePIDFile: filepath.Join(root, "native.pid"),
	})
	if !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("guardian fallback error = %v", err)
	}
	if signalCalls != 0 {
		t.Fatalf("guardian fallback signalled a recycled numeric identity %d times", signalCalls)
	}
}

func TestDarwinFastExitFallbackBranches(t *testing.T) {
	t.Run("group absent", func(t *testing.T) {
		preserveDarwinSupervisorSeams(t)
		config := darwinSupervisorTestConfig(t)
		liveness, err := openLivenessContainment(config)
		if err != nil {
			t.Fatal(err)
		}
		darwinSupervisorGetpgid = func(int) (int, error) { return 0, syscall.ESRCH }
		darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.ESRCH }
		err = liveness.Start(exec.Command("/usr/bin/true"))
		if err == nil || errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("fast-exit result = %v", err)
		}
	})

	t.Run("group present cleanup", func(t *testing.T) {
		preserveDarwinSupervisorSeams(t)
		config := darwinSupervisorTestConfig(t)
		liveness, err := openLivenessContainment(config)
		if err != nil {
			t.Fatal(err)
		}
		darwinSupervisorGetpgid = func(int) (int, error) { return 0, syscall.ESRCH }
		calls := 0
		darwinSupervisorKill = func(int, syscall.Signal) error {
			calls++
			if calls == 1 {
				return nil
			}

			return syscall.ESRCH
		}
		err = liveness.Start(exec.Command("/usr/bin/true"))
		if err == nil || errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("present fast-exit result = %v", err)
		}
	})

	t.Run("probe failure", func(t *testing.T) {
		preserveDarwinSupervisorSeams(t)
		config := darwinSupervisorTestConfig(t)
		liveness, err := openLivenessContainment(config)
		if err != nil {
			t.Fatal(err)
		}
		darwinSupervisorGetpgid = func(int) (int, error) { return 0, syscall.ESRCH }
		darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.EIO }
		err = liveness.Start(exec.Command("/bin/sleep", "10"))
		if !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("probe failure = %v", err)
		}
	})

	t.Run("reap timeout", func(t *testing.T) {
		preserveDarwinSupervisorSeams(t)
		darwinFastExitWait = time.Millisecond
		darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.ESRCH }
		liveness := &livenessContainment{
			generation: &DarwinGeneration{},
			waiter:     &supervisorWaiter{begin: make(chan struct{}), done: make(chan struct{})},
		}
		if err := liveness.handleFastExit(123); !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("fast-exit timeout = %v", err)
		}
	})
}

func TestDarwinStartValidationFailureBranches(t *testing.T) {
	for _, test := range []struct {
		name     string
		getpgid  func(int) (int, error)
		identity bool
	}{
		{name: "getpgid", getpgid: func(int) (int, error) { return 0, syscall.EIO }},
		{name: "wrong group", getpgid: func(pid int) (int, error) { return pid + 1, nil }},
		{name: "identity", getpgid: func(pid int) (int, error) { return pid, nil }, identity: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			preserveDarwinSupervisorSeams(t)
			config := darwinSupervisorTestConfig(t)
			liveness, err := openLivenessContainment(config)
			if err != nil {
				t.Fatal(err)
			}
			darwinSupervisorGetpgid = test.getpgid
			if test.identity {
				darwinProcessIdentityLookup = func(int) (darwinProcessIdentity, error) {
					return darwinProcessIdentity{}, errors.New("identity failed")
				}
				darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.ESRCH }
			}
			err = liveness.Start(exec.Command("/bin/sleep", "10"))
			if test.identity && liveness.process != nil {
				_ = liveness.process.Kill()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, _ = liveness.waiter.await(ctx)
				cancel()
			}
			if !errors.Is(err, ErrProcessContainmentIncomplete) && test.name != "identity" {
				t.Fatalf("validation failure = %v", err)
			}
			if test.identity && err == nil {
				t.Fatal("identity failure was accepted")
			}
		})
	}
}

func TestQuiesceDarwinOriginalGroupBranches(t *testing.T) {
	preserveDarwinSupervisorSeams(t)
	if err := quiesceDarwinOriginalGroupWithProcess(0, nil, nil, nil, time.Second); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("invalid group = %v", err)
	}

	darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if err := quiesceDarwinOriginalGroupWithProcess(123, nil, nil, nil, time.Millisecond); err != nil {
		t.Fatalf("short group cleanup = %v", err)
	}
	if err := quiesceDarwinOriginalGroupWithProcess(123, nil, nil, nil, time.Second); err != nil {
		t.Fatal(err)
	}

	darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.EIO }
	if err := quiesceDarwinOriginalGroupWithProcess(123, nil, nil, nil, time.Second); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("term failure = %v", err)
	}

	calls := 0
	darwinSupervisorKill = func(_ int, signal syscall.Signal) error {
		calls++
		if calls == 1 {
			return syscall.EPERM
		}
		if signal == 0 {
			return syscall.EIO
		}

		return nil
	}
	if err := quiesceDarwinOriginalGroupWithProcess(123, nil, nil, nil, time.Second); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("probe failure = %v", err)
	}

	base := time.Now()
	tick := 0
	darwinSupervisorNow = func() time.Time {
		value := base.Add(time.Duration(tick) * 250 * time.Millisecond)
		tick++

		return value
	}
	darwinSupervisorSleep = func(time.Duration) {}
	calls = 0
	darwinSupervisorKill = func(_ int, signal syscall.Signal) error {
		calls++
		if signal == syscall.SIGKILL {
			return syscall.EIO
		}

		return syscall.EPERM
	}
	if err := quiesceDarwinOriginalGroupWithProcess(123, nil, nil, nil, 5*time.Second); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("kill failure = %v", err)
	}

	tick = 0
	darwinSupervisorKill = func(_ int, signal syscall.Signal) error {
		if signal == syscall.SIGKILL {
			return syscall.ESRCH
		}

		return syscall.EPERM
	}
	if err := quiesceDarwinOriginalGroupWithProcess(123, nil, nil, nil, 5*time.Second); err != nil {
		t.Fatalf("kill ESRCH = %v", err)
	}
}

func TestQuiesceDarwinSignalsBeforeReleasingDirectWaiter(t *testing.T) {
	preserveDarwinSupervisorSeams(t)
	waiter := &supervisorWaiter{begin: make(chan struct{}), done: make(chan struct{})}
	go func() {
		<-waiter.begin
		close(waiter.done)
	}()
	darwinSupervisorKill = func(_ int, signal syscall.Signal) error {
		if signal != syscall.SIGTERM {
			t.Fatalf("first signal = %v, want SIGTERM", signal)
		}
		select {
		case <-waiter.begin:
			t.Fatal("direct waiter was released before the captured group was signalled")
		default:
		}

		return syscall.ESRCH
	}
	if err := quiesceDarwinOriginalGroupWithProcess(123, nil, waiter, nil, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinCleanupSyscallErrorsUseStatefulDirectKillAndJoinSoleWaiter(t *testing.T) {
	for _, failure := range []string{"term", "probe", "kill"} {
		t.Run(failure, func(t *testing.T) {
			preserveDarwinSupervisorSeams(t)
			config := darwinSupervisorTestConfig(t)
			liveness, err := openLivenessContainment(config)
			if err != nil {
				t.Fatal(err)
			}
			if startErr := liveness.Start(exec.Command("/bin/sleep", "30")); startErr != nil {
				t.Fatal(startErr)
			}

			rawPositiveKill := false
			darwinSupervisorKill = func(pid int, signal syscall.Signal) error {
				if pid > 0 && signal == syscall.SIGKILL {
					rawPositiveKill = true
				}

				switch failure {
				case "term":
					if signal == syscall.SIGTERM {
						return syscall.EIO
					}
				case "probe":
					if signal == 0 {
						return syscall.EIO
					}
				case "kill":
					if signal == syscall.SIGKILL {
						return syscall.EIO
					}
					if signal == 0 {
						return syscall.EPERM
					}
				}

				return nil
			}

			err = liveness.Quiesce(liveness.process.Pid, supervisorQuiesceWindow)
			if !errors.Is(err, ErrProcessContainmentIncomplete) {
				t.Fatalf("cleanup error = %v", err)
			}
			if rawPositiveKill {
				t.Fatal("cleanup used a raw positive PID after the group syscall failure")
			}
			select {
			case <-liveness.waiter.done:
			case <-time.After(time.Second):
				t.Fatal("cleanup returned without joining the sole direct-child waiter")
			}
		})
	}
}

func TestDarwinCleanupDoesNotSignalDirectPIDAfterSoleWaiterCompletes(t *testing.T) {
	preserveDarwinSupervisorSeams(t)
	config := darwinSupervisorTestConfig(t)
	liveness, err := openLivenessContainment(config)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sleep", "30")
	if startErr := liveness.Start(command); startErr != nil {
		t.Fatal(startErr)
	}

	realKill := syscall.Kill
	rawPositiveSignal := false
	probeObservedCompletedWaiter := false
	darwinSupervisorKill = func(pid int, signal syscall.Signal) error {
		if pid > 0 {
			rawPositiveSignal = true

			return syscall.EIO
		}

		switch signal {
		case syscall.SIGTERM:
			return realKill(pid, signal)
		case 0:
			select {
			case <-liveness.waiter.done:
				probeObservedCompletedWaiter = true

				return syscall.EIO
			case <-time.After(time.Second):
				return errors.New("direct-child waiter did not complete after group TERM")
			}
		default:
			return realKill(pid, signal)
		}
	}

	err = liveness.Quiesce(liveness.process.Pid, supervisorQuiesceWindow)
	if !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("cleanup error = %v", err)
	}
	if !probeObservedCompletedWaiter {
		t.Fatal("test did not force the group syscall failure after the sole waiter completed")
	}
	if rawPositiveSignal {
		t.Fatal("cleanup signalled a raw positive PID after the sole waiter completed")
	}
	if command.ProcessState == nil {
		t.Fatal("direct child was not reaped")
	}
}

func TestDarwinSetsidEscapeSurvivesSelectedBoundary(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	if role := os.Getenv("ACP_GO_CODEX_DARWIN_CONTAINMENT_HELPER"); role != "" {
		runCodexDarwinSetsidHelper(role)

		return
	}

	parent := t.TempDir()
	pidFile := filepath.Join(parent, "escape.pid")
	config := darwinSupervisorTestConfig(t)
	liveness, err := openLivenessContainment(config)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestDarwinSetsidEscapeSurvivesSelectedBoundary$")
	command.Env = append(os.Environ(),
		"ACP_GO_CODEX_DARWIN_CONTAINMENT_HELPER=parent",
		"ACP_GO_CODEX_DARWIN_CONTAINMENT_PID_FILE="+pidFile,
	)
	if startErr := liveness.Start(command); startErr != nil {
		t.Fatal(startErr)
	}
	t.Cleanup(func() {
		_ = liveness.Quiesce(command.Process.Pid, supervisorQuiesceWindow)
		reapSetsidEscape(t, pidFile)
	})

	var escapedPID int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		raw, readErr := os.ReadFile(pidFile)
		if readErr != nil {
			continue
		}
		escapedPID, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		if err == nil {
			break
		}
	}
	if escapedPID <= 0 {
		t.Fatal("setsid helper did not publish its pid")
	}

	if err := liveness.Quiesce(command.Process.Pid, supervisorQuiesceWindow); err != nil {
		t.Fatal(err)
	}
	if err := <-liveness.Wait(); err == nil {
		t.Fatal("selected-boundary shutdown unexpectedly returned a zero direct-child exit")
	}
	if err := syscall.Kill(escapedPID, 0); err != nil {
		t.Fatalf("setsid descendant did not survive selected-boundary completion: %v", err)
	}
}

// reapSetsidEscape kills the setsid helper the selected boundary deliberately
// leaves running and fails when it survives. It resolves the PID from the
// fixture's own file so it reaps on every exit path, including the t.Fatal
// taken when the helper never publishes a usable PID.
func reapSetsidEscape(t *testing.T, pidFile string) {
	t.Helper()

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return
	}

	_ = syscall.Kill(pid, syscall.SIGKILL)

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
	}

	t.Errorf("setsid escapee pid %d remained after test cleanup", pid)
}

func runCodexDarwinSetsidHelper(role string) {
	switch role {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestDarwinSetsidEscapeSurvivesSelectedBoundary$")
		child.Env = append(os.Environ(), "ACP_GO_CODEX_DARWIN_CONTAINMENT_HELPER=child")
		if err := child.Start(); err != nil {
			os.Exit(91)
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(os.Getenv("ACP_GO_CODEX_DARWIN_CONTAINMENT_PID_FILE")); err == nil {
				os.Exit(0)
			}
		}
		os.Exit(92)
	case "child":
		if _, err := syscall.Setsid(); err != nil {
			os.Exit(93)
		}
		if err := os.WriteFile(os.Getenv("ACP_GO_CODEX_DARWIN_CONTAINMENT_PID_FILE"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(94)
		}
		for {
			time.Sleep(time.Second)
		}
	}
}

func TestDarwinAbortUnvalidatedTimeout(t *testing.T) {
	preserveDarwinSupervisorSeams(t)
	darwinAbortWait = time.Millisecond
	waiter := &supervisorWaiter{begin: make(chan struct{}), done: make(chan struct{})}
	liveness := &livenessContainment{
		generation: &DarwinGeneration{},
		process:    &os.Process{Pid: 99999999},
		waiter:     waiter,
	}
	if err := liveness.abortUnvalidated(nil, errors.New("invalid")); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("abort timeout = %v", err)
	}
}

func TestDarwinAbortUnvalidatedKillsAndReapsAfterTermGrace(t *testing.T) {
	preserveDarwinSupervisorSeams(t)
	darwinAbortWait = 2 * time.Second
	ready := filepath.Join(t.TempDir(), "ready")
	command := exec.Command("/bin/sh", "-c", `trap '' TERM; : > "$1"; exec sleep 30`, "sh", ready)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
			t.Fatal("TERM-ignoring direct child did not become ready")
		}
	}
	waiter := newSupervisorWaiter(command, true)
	liveness := &livenessContainment{
		generation: &DarwinGeneration{},
		process:    command.Process,
		waiter:     waiter,
	}
	started := time.Now()
	err := liveness.abortUnvalidated(nil, errors.New("invalid"))
	if !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("abort result = %v", err)
	}
	if elapsed := time.Since(started); elapsed < darwinGroupTermGrace || elapsed >= darwinAbortWait {
		t.Fatalf("abort elapsed = %v, want TERM grace then reap within %v", elapsed, darwinAbortWait)
	}
	if command.ProcessState == nil {
		t.Fatal("TERM-ignoring direct child was not reaped")
	}
}

func TestAbortDarwinCleanupRemainingBranches(t *testing.T) {
	t.Run("finished direct PID", func(t *testing.T) {
		preserveDarwinSupervisorSeams(t)
		command := exec.Command("/usr/bin/true")
		if err := command.Run(); err != nil {
			t.Fatal(err)
		}
		darwinSupervisorKill = func(int, syscall.Signal) error {
			t.Fatal("completed direct process was signalled through a raw PID")

			return syscall.EIO
		}
		err := abortDarwinCleanup(command.Process, nil, nil, time.Now().Add(time.Second), errors.New("cleanup"))
		if !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("cleanup result = %v", err)
		}
	})

	for _, test := range []struct {
		name     string
		deadline time.Time
	}{
		{name: "expired deadline", deadline: time.Now().Add(-time.Second)},
		{name: "waiter timeout", deadline: time.Now().Add(time.Millisecond)},
	} {
		t.Run(test.name, func(t *testing.T) {
			waiter := &supervisorWaiter{begin: make(chan struct{}), done: make(chan struct{})}
			err := abortDarwinCleanup(nil, waiter, nil, test.deadline, errors.New("cleanup"))
			if !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "remained unreaped") {
				t.Fatalf("cleanup result = %v", err)
			}
		})
	}
}

func TestTerminateIndependentDarwinSupervisorBranches(t *testing.T) {
	preserveDarwinSupervisorSeams(t)
	if err := terminateIndependentSupervisor(nil); err != nil {
		t.Fatal(err)
	}
	cmd := &exec.Cmd{Process: &os.Process{Pid: 99999999}}
	darwinSupervisorKill = func(int, syscall.Signal) error { return nil }
	if err := terminateIndependentSupervisor(cmd); err != nil {
		t.Fatal(err)
	}
	darwinSupervisorKill = func(int, syscall.Signal) error { return os.ErrProcessDone }
	if err := terminateIndependentSupervisor(cmd); err != nil {
		t.Fatal(err)
	}
	want := errors.New("kill failed")
	darwinSupervisorKill = func(int, syscall.Signal) error { return want }
	if err := terminateIndependentSupervisor(cmd); !errors.Is(err, want) {
		t.Fatalf("terminate error = %v", err)
	}
	darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if err := terminateIndependentSupervisor(cmd); err != nil {
		t.Fatalf("gone process = %v", err)
	}
}

func TestDarwinSupervisorProofFailureBranches(t *testing.T) {
	t.Run("guardian cleanup proof", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveDarwinSupervisorSeams(t)
		root := t.TempDir()
		helper := filepath.Join(root, "liveness")
		data := []byte("#!/bin/sh\nprintf '%s\\n' '" + supervisorReadyPrefix + "{\"nativePid\":123}' >&2\nexit 0\n")
		if err := os.WriteFile(helper, data, 0o700); err != nil {
			t.Fatal(err)
		}
		supervisorExecutable = func() (string, error) { return helper, nil }
		supervisorInput = io.NopCloser(&emptyReader{})
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.EIO }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
			LifecycleKind: "runtime", DarwinBestEffort: true,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		})
		if !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("guardian proof error = %v", err)
		}
	})

	t.Run("guardian completion marker", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveDarwinSupervisorSeams(t)
		root := t.TempDir()
		helper := filepath.Join(root, "liveness")
		data := []byte("#!/bin/sh\nprintf '%s\\n' '" + supervisorReadyPrefix + "{\"nativePid\":123}' >&2\nexit 0\n")
		if err := os.WriteFile(helper, data, 0o700); err != nil {
			t.Fatal(err)
		}
		notDirectory := filepath.Join(root, "file")
		if err := os.WriteFile(notDirectory, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		supervisorExecutable = func() (string, error) { return helper, nil }
		supervisorInput = io.NopCloser(&emptyReader{})
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.ESRCH }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
			LifecycleKind: "runtime", DarwinBestEffort: true,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(notDirectory, "complete"),
		})
		if err == nil {
			t.Fatal("guardian accepted completion marker failure")
		}
	})

	t.Run("guardian preserves memoized completion", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveDarwinSupervisorSeams(t)
		root := t.TempDir()
		completion := filepath.Join(root, "complete")
		helper := filepath.Join(root, "liveness")
		data := []byte("#!/bin/sh\nprintf '%s\\n' '" + supervisorReadyPrefix + "{\"nativePid\":123}' >&2\n: > " + completion + "\n")
		if err := os.WriteFile(helper, data, 0o700); err != nil {
			t.Fatal(err)
		}
		supervisorExecutable = func() (string, error) { return helper, nil }
		supervisorInput = io.NopCloser(&emptyReader{})
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		darwinSupervisorKill = func(int, syscall.Signal) error {
			t.Fatal("guardian signalled a numeric PGID after liveness published completion")

			return syscall.EIO
		}
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
			LifecycleKind: "runtime", DarwinBestEffort: true,
			Started: filepath.Join(root, "started"), Completion: completion,
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("liveness natural cleanup", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveDarwinSupervisorSeams(t)
		config := darwinSupervisorTestConfig(t)
		config.NativePath = "/bin/sh"
		config.NativeArgs = []string{"-c", "sleep 0.05"}
		config.NativeEnv = os.Environ()
		config.Home = filepath.Join(config.Scratch, "home")
		config.Started = filepath.Join(config.Scratch, "started")
		config.Completion = filepath.Join(config.Scratch, "complete")
		config.NativePIDFile = filepath.Join(config.Scratch, "pid")
		input, writer := io.Pipe()
		defer writer.Close()
		supervisorInput = input
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.EIO }
		if err := runLiveness(config); !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("liveness proof error = %v", err)
		}
	})
}

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
func (*emptyReader) Close() error             { return nil }
