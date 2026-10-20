# Device key delivery implementation plan

**Goal:** Automatically deliver the existing fc1 content key directly to each newly connected device, supporting several devices per account.

**Architecture:** The authenticated chat API registers a random one-time grant at the key service. The client redeems it directly with a P-256 device signature. Existing account-scoped key caches avoid subsequent delivery. Full compromise of the chat server can authorize another device; the user explicitly accepted this limitation on 2026-10-20.

**Tech stack:** Existing Go services, CryptoKit/Keychain, Kotlin/JCA/Android Keystore, HTTPS and Docker Compose.

**Spec:** User-approved conversation on 2026-10-20; protocol below is the implementation contract.

## Global constraints

- Preserve existing content-key bytes and readable history.
- One grant per `(user_id, device_id)` request; one account can use many devices independently.
- Main API never downloads private content keys. Its existing `ticket_secret` becomes the credential for registering grants only.
- No administrator approval, additional user password, or user-entered delivery code.
- Lost delivery response permits a fresh grant for the same device, never replay of a consumed grant.
- Preserve unrelated iOS signing changes; use existing communication source files where project membership is explicit.

## Protocol

Authenticated GET `/api/v1/encryption/key` returns `scheme`, `key_id`, `key_server_url`, `key_delivery: device-grant-v1`. Legacy nonexternal fc1 delivery remains available for servers without an independent key service.

Authenticated POST `/api/v1/encryption/device-grant` accepts `{device_id,device_public_key}`. Public key is standard-base64 uncompressed P-256 (65 bytes); IDs are canonical lowercase UUIDs. User identity comes from the authenticated session. The chat server creates 32 random bytes encoded as lowercase hex `grant_id`, with `expires_at = now + 120` Unix seconds.

Before responding it POSTs `/api/v1/device-grants` at the configured key-service origin, with `Authorization: Bearer <base64 ticket_secret>`, `X-FamilyChat-Issuer`, and strict JSON `{version:1,grant_id,user_id,device_id,device_public_key,key_id,expires_at}`. Key service returns 204. The client response contains scheme/key/server/delivery metadata, user/device IDs, grant ID and expiry, without RPC credentials.

Client POSTs `/api/v1/keys/current` with `{version:1,user_id,device_id,grant_id,signature}`. Signature is standard-base64 ECDSA/SHA-256 DER over this UTF-8 text, without a trailing newline:

```
familychat-device-grant-v1
<key_id>
<user_id>
<device_id>
<grant_id>
```

The key service checks signature and all bindings, atomically persists consumption, and returns existing fc1 JSON. Consumed grant returns 409; invalid/expired proof returns 401. Bad proof never consumes a grant. Requests use HTTPS with no redirects and bounded bodies. The old JWT-based GET no longer delivers keys.

The writable `/grants` store is separate from read-only `/state`. Pending and consumed grants and their user/device public-key bindings persist until expiry, with bounded cleanup. Local device identities remain stable; the server has no permanent lifetime device-count limit. Existing registration never resurrects a still-valid consumed grant. Issuer always creates new random grant IDs instead of extending old ones.

## Review focus

- Two devices under one account redeem independently and decrypt the same history.
- A replay loses the race even across service restarts; failed signatures do not consume.
- Interrupted response triggers a new grant and no repeated consumption.
- Logout/removing an account clears only its keys and device identity; cancelled requests cannot repopulate its cache.
- Upgrades preserve existing key/TLS bytes and pending deployment recovery state.

## Tasks

- [x] Keyserver Go: persistent grant store, RPC registration, proof verification, legacy rejection; real filesystem, concurrency and restart tests.
- [x] Main Go: descriptor and grant endpoint, bounded nonredirecting RPC, authenticated binding; unit, database integration and independent HTTPS tests.
- [x] Apple: mirrored iOS/macOS device identity, v2 key transfer, account lifecycle and retry tests; native test/build checks.
- [x] Android: Keystore identity, core transfer protocol, cache lifecycle and replay/recovery tests; core tests and APK build.
- [x] Deployment: writable grant mount, install/update/backup handling, protocol capability metadata, two-command upgrade/recovery and installation documentation.
- [ ] Verify cross-service delivery and native signature interoperability, review changes, commit each repository, publish verified server artifacts.
