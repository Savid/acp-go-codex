package codexacp

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/zalando/go-keyring"
)

// authKeystoreService is codex's own keystore service name. The `-local-oauth`
// and `-staging-oauth` variants of the surrounding names are unreachable: the
// environment selector returns the literal production value.
const authKeystoreService = "Codex Auth"

var authKeystoreRead = keyring.Get

// authKeystoreAccount rebuilds codex's keystore account name, which partitions
// the item by CODEX_HOME. Two homes therefore address two different items, and
// a leg that assumed one account per host would harvest another worker's slot.
func authKeystoreAccount(home string) string {
	sum := sha256.Sum256([]byte(home))

	return "cli|" + hex.EncodeToString(sum[:])[:16]
}

// readKeystoreCredential reads the home's keystore item. Keyring mode has no
// file fallback: a home configured for it and holding no item is not logged in,
// however readable the file sitting beside it is.
func readKeystoreCredential(home string) ([]byte, error) {
	secret, err := authKeystoreRead(authKeystoreService, authKeystoreAccount(home))
	if err != nil {
		return nil, err
	}

	return []byte(secret), nil
}
