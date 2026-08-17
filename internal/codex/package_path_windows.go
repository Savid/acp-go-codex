package codex

func stagePackagedCodex(path string, nativeEnv []string, _ string) (string, []string, error) {
	return path, nativeEnv, nil
}

func stagePackagedCodexForProcess(
	path string,
	nativeEnv []string,
	_ string,
	_ string,
	_ *ProcessIsolation,
) (string, []string, string, error) {
	return path, nativeEnv, "", nil
}
