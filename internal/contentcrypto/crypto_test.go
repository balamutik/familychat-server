package contentcrypto

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCryptoKitInteroperability(t *testing.T) {
	raw, err := os.ReadFile("testdata/interop.json")
	if err != nil {
		t.Fatal(err)
	}
	var key Key
	if err = json.Unmarshal(raw, &key); err != nil {
		t.Fatal(err)
	}
	if err = key.Validate(); err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go_ciphertext", "swift_ciphertext"} {
		ciphertext, err := base64.StdEncoding.DecodeString(fields[name])
		if err != nil {
			t.Fatal(err)
		}
		var opened bytes.Buffer
		if err = key.Decrypt(&opened, bytes.NewReader(ciphertext), fields["context"]); err != nil || opened.String() != fields["plaintext"] {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestEnvelopeRoundTripAndIntegrity(t *testing.T) {
	k, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := Generate()
	for _, n := range []int{0, 1, 64000, ChunkSize, ChunkSize + 1, 2 * ChunkSize} {
		plain := bytes.Repeat([]byte{0x61}, n)
		var encrypted bytes.Buffer
		if err := k.Encrypt(&encrypted, bytes.NewReader(plain), int64(n), "message:chat:sender:client"); err != nil {
			t.Fatal(err)
		}
		sealed := encrypted.Bytes()
		if int64(len(sealed)) != StoredSize(int64(n)) {
			t.Fatal("incorrect framing size")
		}
		var opened bytes.Buffer
		if err := k.Decrypt(&opened, bytes.NewReader(sealed), "message:chat:sender:client"); err != nil || !bytes.Equal(opened.Bytes(), plain) {
			t.Fatalf("round trip %d: %v", n, err)
		}
		if n > 32 && bytes.Contains(sealed, plain) {
			t.Fatal("plaintext leaked")
		}
		for _, invalid := range [][]byte{sealed[:len(sealed)-1], append(append([]byte{}, sealed...), 0), append([]byte{}, sealed...)} {
			if len(invalid) == len(sealed) {
				invalid[len(invalid)-1] ^= 1
			}
			if err := k.Decrypt(&bytes.Buffer{}, bytes.NewReader(invalid), "message:chat:sender:client"); err == nil {
				t.Fatal("accepted tampered/truncated envelope")
			}
		}
		if err := k.Decrypt(&bytes.Buffer{}, bytes.NewReader(sealed), "message:other:sender:client"); err == nil {
			t.Fatal("accepted wrong context")
		}
		if err := other.Decrypt(&bytes.Buffer{}, bytes.NewReader(sealed), "message:chat:sender:client"); err == nil {
			t.Fatal("accepted wrong key")
		}
	}
}

func TestKeyFileSurvivesReloadAndDoesNotOverwriteDamage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.json")
	first, err := LoadOrCreate(path, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(path, first.ID)
	if err != nil || first.ID != second.ID {
		t.Fatalf("reload: %v", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("private key permissions")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path, first.ID); err == nil {
		t.Fatal("silently replaced missing key")
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path, ""); err == nil {
		t.Fatal("silently replaced corrupt key")
	}
}
