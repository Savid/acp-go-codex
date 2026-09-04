package codexacp

import "testing"

func TestValidateAbsolutePathsRejectsRelative(t *testing.T) {
	if err := validateAbsolutePaths("paths", []string{"/ok", "relative"}); err == nil {
		t.Fatal("validateAbsolutePaths accepted relative path")
	}
}

func TestValidatePathBranches(t *testing.T) {
	if err := validateRequiredAbsolutePath("cwd", ""); err == nil {
		t.Fatal("validateRequiredAbsolutePath accepted empty path")
	}
	if err := validateRequiredAbsolutePath("cwd", "relative"); err == nil {
		t.Fatal("validateRequiredAbsolutePath accepted relative path")
	}
	if err := validateSessionStartPaths("", nil); err == nil {
		t.Fatal("validateSessionStartPaths accepted empty cwd")
	}
	if err := validateSessionStartPaths(absTestPath("repo"), []string{"relative"}); err == nil {
		t.Fatal("validateSessionStartPaths accepted relative additional directory")
	}
	if err := validateAbsolutePaths("paths", []string{""}); err == nil {
		t.Fatal("validateAbsolutePaths accepted empty path")
	}
	if err := validateOptionalAbsolutePath("cwd", nil); err != nil {
		t.Fatalf("nil optional path returned error: %v", err)
	}
	path := absTestPath("repo")
	if err := validateOptionalAbsolutePath("cwd", &path); err != nil {
		t.Fatalf("absolute optional path returned error: %v", err)
	}
}
