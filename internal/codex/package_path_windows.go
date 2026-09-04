package codex

func stagePackagedCodex(path string, nativeEnv []string, _ string) (string, []string, func() error, error) {
	return path, nativeEnv, func() error { return nil }, nil
}
