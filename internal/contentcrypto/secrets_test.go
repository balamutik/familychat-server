package contentcrypto

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareSecretsPreservesLegacyKeyAndDecryptsOldContent(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "key.json")
	out := filepath.Join(dir, "secrets")
	old, err := LoadOrCreate(legacy, "")
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := old.EncryptText("existing history", "message:test")
	if err != nil {
		t.Fatal(err)
	}
	key, err := PrepareSecretFiles(out, legacy, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSecrets(filepath.Join(out, PublicSecretName), filepath.Join(out, PrivateSecretName), old.ID)
	if err != nil || loaded.ID != key.ID || !bytes.Equal(loaded.Private, old.Private) {
		t.Fatalf("key changed: %v", err)
	}
	sealed, _ := base64.StdEncoding.DecodeString(ciphertext)
	var plain bytes.Buffer
	if err := loaded.Decrypt(&plain, bytes.NewReader(sealed), "message:test"); err != nil || plain.String() != "existing history" {
		t.Fatal("old content cannot be decrypted", err)
	}
	before, _ := os.ReadFile(filepath.Join(out, PrivateSecretName))
	if _, err := PrepareSecretFiles(out, legacy, old.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(out, PrivateSecretName))
	if !bytes.Equal(before, after) {
		t.Fatal("secret rotated")
	}
	for _, name := range []string{PublicSecretName, PrivateSecretName} {
		info, err := os.Stat(filepath.Join(out, name))
		if err != nil || info.Mode().Perm() != 0400 {
			t.Fatal("unsafe secret permissions", err)
		}
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("legacy backup was removed")
	}
}

func TestSecretsFailClosedAndResumePartialPreparation(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "missing.json")
	out := filepath.Join(dir, "secrets")
	if _, err := LoadSecrets(filepath.Join(out, PublicSecretName), filepath.Join(out, PrivateSecretName), ""); err == nil {
		t.Fatal("runtime generated missing secrets")
	}
	if _, err := PrepareSecretFiles(out, legacy, "existing-fingerprint"); err == nil {
		t.Fatal("replaced missing existing key")
	}
	key, err := PrepareSecretFiles(out, legacy, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(out, PublicSecretName)); err != nil {
		t.Fatal(err)
	}
	recovered, err := PrepareSecretFiles(out, legacy, key.ID)
	if err != nil || recovered.ID != key.ID {
		t.Fatal("cannot resume private-only preparation", err)
	}
	other, _ := Generate()
	if _, err := LoadSecrets(filepath.Join(out, PublicSecretName), filepath.Join(out, PrivateSecretName), other.ID); err == nil {
		t.Fatal("wrong DB accepted")
	}
	if err := os.Chmod(filepath.Join(out, PrivateSecretName), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecrets(filepath.Join(out, PublicSecretName), filepath.Join(out, PrivateSecretName), key.ID); err == nil {
		t.Fatal("world-readable private key accepted")
	}
	if err := os.Chmod(filepath.Join(out, PrivateSecretName), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(out, PrivateSecretName)); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSecretFiles(out, legacy, ""); err == nil {
		t.Fatal("public-only state replaced with another key")
	}
	raw, _ := json.Marshal(other)
	if err := os.WriteFile(legacy, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSecretFiles(out, legacy, ""); err == nil {
		t.Fatal("mismatched partial secret overwritten")
	}
}
