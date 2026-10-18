package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"

	"familychat/server/internal/contentcrypto"
)

func main() {
	if len(os.Args) != 4 {
		println("usage: androidinterop key.json android-result.json cross.json")
		os.Exit(2)
	}
	var key contentcrypto.Key
	b, e := os.ReadFile(os.Args[1])
	must(e)
	must(json.Unmarshal(b, &key))
	must(key.Validate())
	var data map[string]string
	b, e = os.ReadFile(os.Args[2])
	must(e)
	must(json.Unmarshal(b, &data))
	for _, name := range []string{"android_ciphertext", "swift_ciphertext"} {
		encoded, exists := data[name]
		if !exists {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		must(err)
		var out bytes.Buffer
		must(key.Decrypt(&out, bytes.NewReader(raw), data["context"]))
		if out.String() != data["plaintext"] {
			panic("Interoperability plaintext mismatch")
		}
	}
	data["go_ciphertext"], e = key.EncryptText(data["plaintext"], data["context"])
	must(e)
	b, e = json.Marshal(data)
	must(e)
	must(os.WriteFile(os.Args[3], b, 0600))
	must(os.Chmod(os.Args[3], 0600))
	println("Go decrypted fresh Android ciphertext")
}
func must(e error) {
	if e != nil {
		panic(e)
	}
}
