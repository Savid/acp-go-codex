//go:build !unix

package codex

import "errors"

func currentProcessIdentity() (uint32, uint32, error) {
	return 0, 0, errors.New("current process identity is unavailable")
}
