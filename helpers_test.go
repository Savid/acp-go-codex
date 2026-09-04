package codexacp

import (
	"testing"
	"time"
)

// runtimeGenerationSnapshot reads the shared app-server generation a test wants
// to prove survived, or did not. The epoch is what distinguishes a generation
// that kept serving from a replacement started after one was fenced.
type runtimeGeneration struct {
	epoch uint64
	dead  bool
}

func (a *Agent) runtimeGenerationSnapshot() runtimeGeneration {
	a.mu.Lock()
	defer a.mu.Unlock()

	return runtimeGeneration{epoch: a.runtimeEpoch, dead: a.runtimeDead}
}

// testSignalTimeout bounds every rendezvous a test waits on. It is generous
// enough that a slow or loaded machine never trips it, and short enough that a
// signal which is never going to arrive is reported as a failure long before the
// package timeout would take the rest of the suite down with it.
const testSignalTimeout = 30 * time.Second

// awaitTestSignal takes one value from a rendezvous a test is waiting on. A
// signal that never arrives fails the test at the point it waited, naming what
// it waited for, instead of parking the package until its timeout and hiding
// every test that had not run yet behind the one that hung.
func awaitTestSignal[T any](t *testing.T, signal <-chan T, what string) T {
	t.Helper()

	select {
	case value := <-signal:
		return value
	case <-time.After(testSignalTimeout):
		t.Fatalf("timed out waiting for %s", what)

		var zero T

		return zero
	}
}
