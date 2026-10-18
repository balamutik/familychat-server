# Fresh Android / Go / CryptoKit interoperability

Run this helper only with a **dedicated QA key**, never the production content key. `android-result.json` is exported by Android's opt-in `ServerE2ETest` and contains synthetic plaintext, ciphertext and its exact message context. No auth token is exported.

```sh
go run ./tests/integration/androidinterop /secure/qa-key.json /tmp/android-result.json /tmp/cross.json
```

The helper validates/decrypts fresh Android ciphertext and emits fresh Go ciphertext. Add a `swift_ciphertext` generated with the same context/plaintext by the unchanged `ios/Communication/ContentCipher.swift` implementation to verify that direction as well. `FreshInteropTest` consumes this combined file through the Android QA runner's `--cross` option. Inputs/outputs belong outside Git; output mode is0600. Existing committed interoperability fixtures remain independent of this opt-in live check.
