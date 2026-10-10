// Test-only adapter for the Python live Compose smoke test. Secrets are read
// from stdin, never command arguments or logs. It is not part of the server image.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"familychat/server/internal/contentcrypto"
)

func main() {
	var in struct {
		Key     contentcrypto.Key `json:"key"`
		Data    []byte            `json:"data"`
		Context string            `json:"context"`
		Decrypt bool              `json:"decrypt"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		fail()
	}
	if in.Key.Validate() != nil {
		fail()
	}
	var out bytes.Buffer
	var err error
	if in.Decrypt {
		err = in.Key.Decrypt(&out, bytes.NewReader(in.Data), in.Context)
	} else {
		err = in.Key.Encrypt(&out, bytes.NewReader(in.Data), int64(len(in.Data)), in.Context)
	}
	if err != nil {
		fail()
	}
	if json.NewEncoder(os.Stdout).Encode(map[string][]byte{"data": out.Bytes()}) != nil {
		fail()
	}
}

func fail() { fmt.Fprintln(os.Stderr, "test encryption failed"); os.Exit(1) }
