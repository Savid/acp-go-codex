package codexacp

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
