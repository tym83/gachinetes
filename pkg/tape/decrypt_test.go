package tape

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/nacl/secretbox"
)

func TestDecrypter(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cfg := `apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
- resources: [secrets]
  providers:
  - secretbox:
      keys:
      - {name: key1, secret: ` + base64.StdEncoding.EncodeToString(key) + `}
  - aesgcm:
      keys:
      - {name: g, secret: ` + base64.StdEncoding.EncodeToString(key) + `}
  - identity: {}
`
	path := filepath.Join(t.TempDir(), "enc.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDecrypter(path)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("k8s\x00secret bytes")
	var nonce [24]byte
	var sk [32]byte
	_, _ = rand.Read(nonce[:])
	copy(sk[:], key)
	box := append([]byte("k8s:enc:secretbox:v1:key1:"), secretbox.Seal(nonce[:], plain, &nonce, &sk)...)
	if got, err := d.Open("/registry/secrets/a/b", box); err != nil || string(got) != string(plain) {
		t.Fatalf("secretbox: %q %v", got, err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	n := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(n)
	sealed := append([]byte("k8s:enc:aesgcm:v1:g:"), gcm.Seal(n, n, plain, []byte("/registry/secrets/a/b"))...)
	if got, err := d.Open("/registry/secrets/a/b", sealed); err != nil || string(got) != string(plain) {
		t.Fatalf("aesgcm: %q %v", got, err)
	}
	if _, err := d.Open("/registry/secrets/a/OTHER", sealed); err == nil {
		t.Fatal("aesgcm opened under another key")
	}
	if got, _ := d.Open("k", plain); string(got) != string(plain) {
		t.Fatal("a plain value changed")
	}
	if _, err := d.Open("k", []byte("k8s:enc:kms:v2:x:yyy")); err == nil {
		t.Fatal("kms opened")
	}
}
