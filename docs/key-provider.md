# KeyProvider — Bring Your Own KMS

`acp-server` encrypts personally identifiable platform identifiers at rest
(ACR-700). The actual master-key storage is **pluggable**. The reference
build ships a single implementation, `LocalKeyfileProvider`, that keeps the
keyring on local disk under `keys/v{N}.key`. Anything beyond that — AWS KMS,
HashiCorp Vault, GCP KMS, Azure Key Vault, an HSM — is meant to be written
by the operator who deploys the server.

This document explains why, what the contract is, and how to plug in your own.

## Why no built-in KMS adapter

We deliberately do not ship `AWSKMSProvider`, `VaultProvider`, etc. as part
of the core server. Two reasons:

1. **Trust model.** No enterprise wants to encrypt their data against a
   key held by a third party — including us. A KMS adapter is only useful
   when it talks to *your* KMS, in *your* account, under *your* IAM policy.
   We have no business being in that path.
2. **API shape uncertainty.** Each KMS has its own quirks: token rotation
   (Vault), envelope encryption depth (AWS KMS), regional failover (GCP),
   audit log shape, IAM error envelopes. An adapter written without a real
   user picks the wrong defaults. We would rather wait for a concrete
   integration request than ship a vapor adapter.

The `KeyProvider` interface is the extension point. Every at-rest
encryption call site depends on it, never on `LocalKeyfileProvider`
directly, so a custom adapter is a strict drop-in replacement.

## The contract

```go
package keys

const KeySize = 32 // AES-256 master key, ACR-700 §2.2

type KeyProvider interface {
    // Current returns the active (key, key_version) pair for new writes.
    Current() (key [KeySize]byte, version uint32, err error)

    // At returns the historical key for decrypting an older row. Returns
    // ErrKeyVersionUnavailable if the version is archived beyond reach.
    At(version uint32) (key [KeySize]byte, err error)
}
```

Defined in [`internal/keys/keys.go`](../internal/keys/keys.go).

### Invariants every implementation must hold

| Rule | Why it matters |
|------|----------------|
| `Current()` and `At()` are safe for concurrent use. | The HTTP handler pool calls them from many goroutines per request. |
| `version` is a monotonically increasing `uint32` ≥ 1. The on-disk header reserves only 24 bits, so values must stay below `1 << 24`. | The §2.3 ciphertext header packs `key_version` into a big-endian u24. |
| `At(v)` for any version that ever appeared in `Current()` must keep returning the same bytes for the lifetime of the deployment. | Old `*_enc` rows are decrypted by replaying the version recorded in their header. Losing a key permanently breaks every row sealed under it — by design. |
| `At(v)` for an unavailable version must return `ErrKeyVersionUnavailable`. Do not fall back to `Current()` and do not return zero bytes. | Decrypt paths use `errors.Is(err, ErrKeyVersionUnavailable)` to distinguish "ciphertext is fine but the archived key is gone" from "ciphertext is corrupt". |
| Keys must come from a CSPRNG, not a passphrase or a derived value. | ACR-700 §2.2. AES-256-GCM nonce uniqueness assumes a uniformly random key. |

### Optional capability: rotation

`KeyProvider` itself does not require a `Rotate()` method — the `acp-server
rotate-key` subcommand is wired specifically to `LocalKeyfileProvider`. If
your adapter is backed by a managed KMS, rotation is usually triggered out
of band (a KMS console action, an IAM-scoped CLI call, or a Terraform
change). Your adapter just needs to *observe* the new version on next
startup or via a refresh hook.

If you want to expose `acp-server rotate-key` for your backend, give your
type a `Rotate() (uint32, string, error)` method and pass it where the
subcommand expects it. The signature returns `(new_version,
fingerprint_hex, err)`.

## Writing an adapter — skeleton

```go
package mykms

import (
    "context"

    "github.com/inkmesh/acp-server/internal/keys"
)

// Adapter wraps a KMS client. The client is whatever your provider hands
// you — keep this struct narrow.
type Adapter struct {
    client *kmsClient
    keyARN string
}

func New(client *kmsClient, keyARN string) *Adapter {
    return &Adapter{client: client, keyARN: keyARN}
}

func (a *Adapter) Current() ([keys.KeySize]byte, uint32, error) {
    // 1. Ask the KMS for the active version under a.keyARN.
    // 2. Fetch the raw key material (or perform envelope decryption).
    // 3. Copy it into a [32]byte and return alongside the version number.
    //
    // This call is hot — it runs on every Seal. Cache aggressively, but
    // make sure invalidation is correct on rotation events.
}

func (a *Adapter) At(version uint32) ([keys.KeySize]byte, error) {
    // 1. Ask the KMS for the specified version.
    // 2. If the KMS reports "version not found" or equivalent,
    //    return keys.ErrKeyVersionUnavailable verbatim.
    // 3. Otherwise return the bytes.
}
```

Wiring it into the server is one line in `main.go` (replacing
`keys.NewLocalKeyfileProvider(...)`).

### Caching guidance

KMS round-trips are slow relative to a single AEAD operation. Most
adapters cache the materialised key in process memory:

- Cache `Current()` indefinitely; invalidate when you observe a rotation
  (push notification, periodic poll, or a SIGHUP-triggered reload).
- Cache `At()` per version; entries are immutable for the deployment's
  lifetime, so the cache only grows.
- Zeroize cache entries on shutdown if your KMS contract requires it.

Caching is your responsibility, not the interface's. The reference
`LocalKeyfileProvider` just keeps everything in a `map[uint32][32]byte`.

## What the protocol does NOT depend on

A few things are intentionally outside the `KeyProvider` contract so
adapters stay small:

- **Header format.** The `[ver:1|key_ver:3 BE u24|nonce:12|ct+tag]` layout
  lives in `internal/crypto/seal.go`. Adapters never see ciphertext.
- **AAD construction.** `"acp-server|" + row_id + "|" + column` is
  built and authenticated by `Sealer`. Adapters supply keys, not policy.
- **Reencryption walk.** `internal/reencrypt` walks `*_enc` columns and
  drives `Open` → `Seal` → `UPDATE`. Your adapter is invoked through the
  same `Sealer` and stays oblivious.

This keeps the surface area you have to test small — round-trip a
`Current()` and `At()` call against a mock KMS and you've covered the
contract.

## When we would ship a built-in adapter

The roadmap parks built-in `AWSKMSProvider` / `VaultProvider` behind a
single gate: **a deploying organisation asks for it**. If you are
operating `acp-server` against a managed KMS and would prefer not to
maintain the adapter yourself, open an issue describing the backend, the
rotation policy, and the IAM shape you need. That is the signal that
unblocks the work — not a hypothetical demand.
