//go:build !darwin

package codex

import (
	"errors"
	"io"
)

func DiagnoseDarwinContainment(string, io.Writer) error {
	return errors.New("containment diagnose is available only on darwin")
}

func CleanupDarwinContainment(string, string, bool, io.Writer) error {
	return errors.New("containment cleanup is available only on darwin")
}
