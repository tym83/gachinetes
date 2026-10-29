package tape

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/nacl/secretbox"
	"sigs.k8s.io/yaml"
)

// Decrypter opens what the API server encrypted at rest, with the keys of
// its encryption configuration: secretbox, aescbc and aesgcm. Values
// encrypted by a KMS stay closed.
type Decrypter struct {
	keys map[string][]byte // "provider:name" -> key
}

type encryptionConfig struct {
	Resources []struct {
		Providers []map[string]struct {
			Keys []struct {
				Name   string `json:"name"`
				Secret string `json:"secret"`
			} `json:"keys"`
		} `json:"providers"`
	} `json:"resources"`
}

// LoadDecrypter reads the API server's encryption configuration.
func LoadDecrypter(path string) (*Decrypter, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg encryptionConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	d := &Decrypter{keys: map[string][]byte{}}
	for _, r := range cfg.Resources {
		for _, p := range r.Providers {
			for provider, conf := range p {
				for _, k := range conf.Keys {
					key, err := base64.StdEncoding.DecodeString(k.Secret)
					if err != nil {
						return nil, fmt.Errorf("%s: key %s of %s: %w", path, k.Name, provider, err)
					}
					d.keys[provider+":"+k.Name] = key
				}
			}
		}
	}
	return d, nil
}

// Open decrypts a value stored under key; values not encrypted come back
// as they are.
func (d *Decrypter) Open(key string, value []byte) ([]byte, error) {
	if d == nil || !bytes.HasPrefix(value, encMagic) {
		return value, nil
	}
	// k8s:enc:<provider>:v1:<name>:<ciphertext>
	rest := value[len(encMagic):]
	parts := bytes.SplitN(rest, []byte(":"), 4)
	if len(parts) != 4 {
		return nil, errors.New("not an encrypted value the API server writes")
	}
	provider, name, data := string(parts[0]), string(parts[2]), parts[3]
	k, ok := d.keys[provider+":"+name]
	if !ok {
		return nil, fmt.Errorf("%w with %s key %s, which the tape does not hold", ErrEncrypted, provider, name)
	}
	switch provider {
	case "secretbox":
		if len(k) != 32 || len(data) < 24 {
			return nil, errors.New("secretbox: bad key or value")
		}
		var nonce [24]byte
		var sk [32]byte
		copy(nonce[:], data[:24])
		copy(sk[:], k)
		out, ok := secretbox.Open(nil, data[24:], &nonce, &sk)
		if !ok {
			return nil, errors.New("secretbox: the value does not open with the key")
		}
		return out, nil
	case "aescbc":
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		if len(data) < aes.BlockSize || len(data)%aes.BlockSize != 0 {
			return nil, errors.New("aescbc: bad value")
		}
		iv, ct := data[:aes.BlockSize], append([]byte(nil), data[aes.BlockSize:]...)
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(ct, ct)
		pad := int(ct[len(ct)-1])
		if pad == 0 || pad > aes.BlockSize || pad > len(ct) {
			return nil, errors.New("aescbc: bad padding")
		}
		return ct[:len(ct)-pad], nil
	case "aesgcm":
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		n := gcm.NonceSize()
		if len(data) < n {
			return nil, errors.New("aesgcm: bad value")
		}
		// The API server binds the value to its key in the store.
		return gcm.Open(nil, data[:n], data[n:], []byte(key))
	}
	return nil, fmt.Errorf("%w by %s", ErrEncrypted, strings.TrimSpace(provider))
}
