package codex

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	envCodexHome    = "CODEX_HOME"
	envCodexPath    = "CODEX_EXECUTABLE"
	minCodexVersion = "0.134.0"
)

var execCommandContext = exec.CommandContext
var processCloseGrace = 2 * time.Second

func launchAppServer(ctx context.Context, options Options) (*lineTransport, *exec.Cmd, error) {
	path, err := resolveCodexPath(options.CLIPath)
	if err != nil {
		return nil, nil, err
	}
	if err := validateCodexVersion(ctx, path); err != nil {
		return nil, nil, err
	}

	args := []string{"app-server", "--listen", "stdio://", "--disable", "plugins"}
	for key, value := range options.Config {
		args = append(args, "-c", fmt.Sprintf("%s=%s", key, shellValue(value)))
	}
	args = append(args, options.ExtraArgs...)

	cmd := execCommandContext(ctx, path, args...)
	cmd.Env = mergedEnv(options)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, err
	}
	cmd.Stderr = codexStderrWriter(options.Logger)

	if err := startProcess(cmd); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, nil, err
	}

	return newLineTransport(stdout, stdin, processCloser{cmd: cmd, stdin: stdin, stdout: stdout}), cmd, nil
}

type processStderrWriter struct {
	logger *slog.Logger
}

func codexStderrWriter(logger *slog.Logger) io.Writer {
	if logger == nil {
		logger = slog.Default()
	}

	return processStderrWriter{logger: logger}
}

func (w processStderrWriter) Write(p []byte) (int, error) {
	text := strings.TrimSpace(string(p))
	if text != "" {
		w.logger.DebugContext(
			context.Background(),
			"Codex app-server stderr",
			slog.String("stderr", strings.TrimRight(string(p), "\r\n")),
		)
	}

	return len(p), nil
}

func resolveCodexPath(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return path, nil
	}
	if env := strings.TrimSpace(os.Getenv(envCodexPath)); env != "" {
		return env, nil
	}

	resolved, err := exec.LookPath("codex")
	if err != nil {
		return "", fmt.Errorf("find codex CLI: %w", err)
	}

	return resolved, nil
}

func validateCodexVersion(ctx context.Context, path string) error {
	cmd := execCommandContext(ctx, path, "--version")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("check codex CLI version: %w", err)
	}
	version := parseCodexVersion(string(output))
	if version == "" {
		return fmt.Errorf("check codex CLI version: could not parse %q", strings.TrimSpace(string(output)))
	}
	if compareSemver(version, minCodexVersion) < 0 {
		return fmt.Errorf("codex CLI %s is too old; need >= %s", version, minCodexVersion)
	}

	return nil
}

var codexVersionRE = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?`)

func parseCodexVersion(output string) string {
	match := codexVersionRE.FindString(output)
	if match == "" {
		return ""
	}
	if cut, _, ok := strings.Cut(match, "-"); ok {
		match = cut
	}
	if cut, _, ok := strings.Cut(match, "+"); ok {
		match = cut
	}

	return match
}

func compareSemver(left string, right string) int {
	leftParts := semverParts(left)
	rightParts := semverParts(right)
	for i := 0; i < len(leftParts); i++ {
		switch {
		case leftParts[i] < rightParts[i]:
			return -1
		case leftParts[i] > rightParts[i]:
			return 1
		}
	}

	return 0
}

func semverParts(value string) [3]int {
	var out [3]int
	parts := strings.Split(value, ".")
	for i := 0; i < len(out) && i < len(parts); i++ {
		part := parts[i]
		out[i], _ = strconv.Atoi(part)
	}

	return out
}

func mergedEnv(options Options) []string {
	env := os.Environ()
	if options.CodexHome != "" {
		env = upsertEnv(env, envCodexHome, options.CodexHome)
	}
	for key, value := range options.Env {
		env = upsertEnv(env, key, value)
	}

	return env
}

func upsertEnv(env []string, key string, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}

	return append(env, prefix+value)
}

func shellValue(value any) string {
	switch typed := value.(type) {
	case string:
		return fmt.Sprintf("%q", typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(typed)
	}
}

type processCloser struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (c processCloser) Close() error {
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.stdout != nil {
		_ = c.stdout.Close()
	}
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}

	done := make(chan error, 1)
	go func() {
		defer recoverCodexGoroutine(context.Background(), "Codex process waiter")
		done <- c.cmd.Wait()
	}()

	select {
	case err := <-done:
		return processCloseError(err)
	case <-time.After(processCloseGrace):
		if err := killProcess(c.cmd); err != nil {
			return err
		}
		<-done
		return nil
	}
}
