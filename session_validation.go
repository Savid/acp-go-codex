package codexacp

import (
	"path/filepath"

	"github.com/coder/acp-go-sdk"
)

const validationAbsolutePath = "must be an absolute path"

// validateRequiredAbsolutePath refuses a path the adapter cannot resolve
// identically from its own working directory. Absent and relative are one
// verdict on one field: the value the caller supplied is not one this surface
// accepts, reported in the uniform `{error, field}` shape rather than as a
// message keyed by the field name.
func validateRequiredAbsolutePath(field string, path string) error {
	if !filepath.IsAbs(path) {
		return acp.NewInvalidParams(map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: field})
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
				jsonFieldPath:  path,
				jsonFieldError: validationAbsolutePath,
			}})
		}
	}

	return nil
}
