package codexacp

import "testing"

func TestValidateAbsolutePathsRejectsRelative(t *testing.T) {
	if err := validateAbsolutePaths("paths", []string{"/ok", "relative"}); err == nil {
		t.Fatal("validateAbsolutePaths accepted relative path")
	}
}
