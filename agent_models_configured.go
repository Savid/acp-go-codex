package codexacp

import (
	"fmt"
	"strings"
)

// validateConfiguredModels refuses an id that could not name a model: empty,
// carrying surrounding space, or listed twice.
func validateConfiguredModels(ids []string) error {
	seen := make(map[string]struct{}, len(ids))
	for index, id := range ids {
		if id == "" || strings.TrimSpace(id) != id {
			return fmt.Errorf("configured model %d %q is not a model id", index, id)
		}

		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("configured model %q is listed twice", id)
		}

		seen[id] = struct{}{}
	}

	return nil
}
