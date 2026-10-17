// Package vault шифрует секреты (API-ключи, cookies) с помощью AES-256-GCM.
// Мастер-ключ хранится в системной связке ключей (macOS Keychain) и никогда
// не попадает в базу данных.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

const (
	keyringService = "dev.edy.ide"
	keyringUser    = "master-key"
	keySize        = 32
)

// Vault шифрует и расшифровывает произвольные байты одним мастер-ключом.
type Vault struct {
	aead cipher.AEAD
}

// New создаёт Vault из 32-байтного мастер-ключа.
func New(masterKey []byte) (*Vault, error) {
	if len(masterKey) != keySize {
		return nil, fmt.Errorf("vault: мастер-ключ должен быть %d байт, получено %d", keySize, len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead}, nil
}

// FromKeychain достаёт мастер-ключ из системной связки ключей, а при первом
// запуске генерирует новый и сохраняет его туда.
func FromKeychain() (*Vault, error) {
	encoded, err := keyring.Get(keyringService, keyringUser)
	if errors.Is(err, keyring.ErrNotFound) {
		key := make([]byte, keySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		encoded = base64.StdEncoding.EncodeToString(key)
		if err := keyring.Set(keyringService, keyringUser, encoded); err != nil {
			return nil, fmt.Errorf("vault: не удалось сохранить мастер-ключ в Keychain: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("vault: не удалось прочитать мастер-ключ из Keychain: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("vault: повреждённый мастер-ключ в Keychain: %w", err)
	}
	return New(key)
}

// Seal шифрует plaintext. Результат: nonce || ciphertext.
func (v *Vault) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return v.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open расшифровывает данные, полученные из Seal.
func (v *Vault) Open(sealed []byte) ([]byte, error) {
	n := v.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("vault: зашифрованные данные слишком короткие")
	}
	return v.aead.Open(nil, sealed[:n], sealed[n:], nil)
}
