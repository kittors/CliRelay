package contentmoderation

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

// A tenant's placeholder key is stored sealed under the server's data key when
// the operator sets one (CLIRELAY_DATA_KEY: 32 random bytes in base64), so a
// copy of the database alone does not let anyone compute the placeholders of
// candidate values. Without it the key is stored as it is, as before, and the
// settings page says so. The data key lives only in the process environment;
// losing it loses the sealed keys, which costs the tenants their placeholders
// (and the prompt caches built on them), not their data: a new key is made
// when masking is switched off and on again.

const dataKeyEnv = "CLIRELAY_DATA_KEY"

var (
	dataKeyOnce  sync.Once
	dataKeyBytes []byte
	// dataKeyForTest replaces the environment in tests.
	dataKeyForTest []byte
)

// dataKey is the server's data key, nil when none is set or the one set is
// unusable (logged once).
func dataKey() []byte {
	if dataKeyForTest != nil {
		return dataKeyForTest
	}
	dataKeyOnce.Do(func() {
		raw := strings.TrimSpace(os.Getenv(dataKeyEnv))
		if raw == "" {
			return
		}
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(key) != 32 {
			log.Errorf("privacy masking: %s is set but is not 32 bytes in base64; tenant keys are stored unsealed", dataKeyEnv)
			return
		}
		dataKeyBytes = key
	})
	return dataKeyBytes
}

// DataKeyConfigured says tenant keys are stored sealed.
func DataKeyConfigured() bool { return dataKey() != nil }

// keyAAD binds a sealed key to its tenant: a sealed key copied into another
// tenant's row does not open.
func keyAAD(tenantID string) []byte {
	return []byte("clirelay privacy-mask key v1\x00" + tenantID)
}

func dataKeyAEAD() (cipher.AEAD, error) {
	key := dataKey()
	if key == nil {
		return nil, errors.New("no data key is set")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealTenantKey seals key for tenantID: base64 of the nonce and the sealed
// bytes.
func sealTenantKey(tenantID string, key []byte) (string, error) {
	aead, err := dataKeyAEAD()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, key, keyAAD(tenantID))), nil
}

// openTenantKey opens what sealTenantKey sealed.
func openTenantKey(tenantID, sealed string) ([]byte, error) {
	aead, err := dataKeyAEAD()
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < aead.NonceSize() {
		return nil, errors.New("the sealed key is malformed")
	}
	return aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], keyAAD(tenantID))
}
