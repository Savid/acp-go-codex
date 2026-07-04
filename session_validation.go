package codexacp

import (
	"path/filepath"

	"github.com/coder/acp-go-sdk"
)

const validationAbsolutePath = "must be an absolute path"

func validateRequiredAbsolutePath(field string, path string) error {
	if path == "" {
		return acp.NewInvalidParams(map[string]any{field: validationRequired})
	}
	if !filepath.IsAbs(path) {
		return acp.NewInvalidParams(map[string]any{field: validationAbsolutePath})
	}

	return nil
}

func validateSessionStartPaths(cwd string, additionalDirectories []string) error {
	if err := validateRequiredAbsolutePath(jsonFieldCwd, cwd); err != nil {
		return err
	}

	return validateAbsolutePaths("additionalDirectories", additionalDirectories)
}

func validateOptionalAbsolutePath(field string, value *string) error {
	if value == nil {
		return nil
	}

	return validateRequiredAbsolutePath(field, *value)
}

func validateAbsolutePaths(field string, paths []string) error {
	for index, path := range paths {
		if path == "" {
			return acp.NewInvalidParams(map[string]any{field: map[string]any{
				jsonFieldIndex: index,
				jsonFieldError: validationRequired,
			}})
		}
		if !filepath.IsAbs(path) {
			return acp.NewInvalidParams(map[string]any{field: map[string]any{
				jsonFieldIndex: index,
				"path":         path,
				jsonFieldError: validationAbsolutePath,
			}})
		}
	}

	return nil
}
