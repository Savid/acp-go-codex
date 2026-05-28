package codexacp

import "github.com/coder/acp-go-sdk"

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
		if err := validateRequiredAbsolutePath(field, path); err != nil {
			return acp.NewInvalidParams(map[string]any{field: map[string]any{
				jsonFieldIndex: index,
				jsonFieldError: err.Error(),
			}})
		}
	}

	return nil
}
