# End-to-end encryption (1:1 direct messages)

**Audience:** split. The [protocol specification](#protocol-specification) is
for developers and reviewers; [Operator notes](#operator-notes) and the
[threat model](#threat-model) are written for everyone, operators included.

Only **1:1 direct messages** are end-to-end encrypted. Group channel messages
are stored server-side and are readable by the server. The DM crypto core lives
in [`internal/e2ee`](../internal/e2ee) (pure, I/O-free, exhaustively tested);
the client-side state machine that drives it over gRPC is
[`internal/client/dm.go`](../internal/client/dm.go).

## Summary

A conversation establishes a session with a synchronous **triple-DH** handshake
over X25519, then exchanges messages framed with **ChaCha20-Poly1305** using
per-direction keys and counter nonces. The server only relays opaque envelopes:
it never sees plaintext, never holds a private key, and never persists DM
traffic. Peer identity keys are pinned **trust-on-first-use** per
`(server, peer)`.

---

## Protocol specification

### Keys

- **Identity key (IK):** a long-term X25519 keypair, generated per server on
  first login and stored locally with mode `0600`
  ([`internal/client/keystore.go`](../internal/client/keystore.go)). The public
  half is published to the server's directory via `PublishIdentityKey`; peers
  fetch it with `GetIdentityKey`.
- **Ephemeral key (EPK):** a fresh X25519 keypair generated per handshake.
  Sessions can only be produced by a completed handshake — there is no other
  constructor and no rekey — so a given AEAD key is never reused across
  sessions and counter nonces can never repeat under a key.

### Roles

The initiator (sender of `SESSION_INIT`) is always **A**; the responder is
**B**. Roles are fixed for the duration and determine which key encrypts in
which direction.

### Handshake (three DHs)

Carried as `DirectEnvelope`s ([chat.proto](../proto/quorum/v1/chat.proto)):

```
A ──SESSION_INIT  (payload = A's ephemeral public key)──▶ B
A ◀──SESSION_ACCEPT(payload = B's ephemeral public key)── B
                       (session established both sides)
```

`Accept` performs B's entire side in one step (generate ephemeral, derive keys,
return the public ephemeral). The three Diffie-Hellman computations:

| DH | Inputs | Purpose |
| --- | --- | --- |
| dh1 | EPK_A · EPK_B | **Forward secrecy** — independent of long-term keys. |
| dh2 | IK_A · EPK_B | Authenticates A to B. |
| dh3 | EPK_A · IK_B | Authenticates B to A. |

All three run inside `deriveKeys`
([`session.go`](../internal/e2ee/session.go)); any failure aborts the whole
derivation. Go's `crypto/ecdh` rejects low-order points (all-zero shared
secrets), so a contributory-behavior attack can't force a known key.

### Key derivation and transcript binding

The three shared secrets are concatenated `dh1‖dh2‖dh3` as HKDF input keying
material. The HKDF **salt** is `SHA-256` over a length-prefixed transcript, in
canonical A-then-B order:

```
protocolLabel ("quorum-e2ee-v1")
serverID                       # SHA-256 of server TLS SPKI
initiatorID, initiatorUsername
responderID, responderUsername
IK_A, IK_B, EPK_A, EPK_B
```

From the HKDF PRK, three values are expanded with distinct info labels:

- `"a2b"` → 32-byte ChaCha20-Poly1305 key (A→B direction)
- `"b2a"` → 32-byte key (B→A direction)
- `"sid"` → 16-byte session ID

Binding the **server identity and both usernames** into the salt means a
handshake transcript from one server (or one pair of users) can never be
replayed or substituted into another context — the derived keys simply won't
match. Usernames are server-local; identities, pins, and transcripts are all
scoped by the server fingerprint.

### Message frames

Each `TYPE_MESSAGE` envelope carries a ciphertext `payload` and an explicit
`counter`. Per direction:

- **Nonce:** 12 bytes, all zero except the low 8 bytes holding the big-endian
  `counter`.
- **AAD:** `sessionID (16) ‖ senderID ‖ counter (8 BE)`. Binding the sender ID
  and counter into the AAD ties each frame to its session, direction, and
  position.
- **Counters are strictly increasing.** `Open` rejects any counter below the
  next expected value (`ErrReplay`) *before* attempting decryption, so replays
  and regressions are dropped. The send counter is the next value to use; the
  receive counter is the lowest acceptable.

`Seal` returns `(ciphertext, counter)`; the caller must put the counter on the
wire. `Close` zeroizes both keys and renders the session unusable.

### Session lifecycle on the wire

`DirectEnvelope.Type` values:

| Type | Payload | Meaning |
| --- | --- | --- |
| `SESSION_INIT` | A's ephemeral pubkey | start a handshake |
| `SESSION_ACCEPT` | B's ephemeral pubkey | complete it |
| `MESSAGE` | ChaCha20-Poly1305 ciphertext | a message (`counter` set) |
| `SESSION_CLOSE` | — | tear the session down |

The server overwrites `sender_id`/`sender_name` from the authenticated identity
on every relay, so the *envelope's* sender can't be forged even though the
payload is opaque.

### Client state machine (fail-closed)

[`dm.go`](../internal/client/dm.go) holds established sessions, in-flight
handshakes, and **in-memory** plaintext queues. The invariants:

- Queued plaintext is flushed **only** after the handshake completes. Any
  failure — handshake error, peer offline, TOFU mismatch, delivery error —
  drops the queue. Plaintext is never sent any other way.
- A **TOFU mismatch blocks the handshake itself**: no session, no send.
- **Simultaneous `SESSION_INIT`s** resolve deterministically with no timing
  dependence: the peer with the lexicographically lower user ID is the rightful
  initiator; the other INIT is discarded (its queued text carries over to the
  winning session).
- A decrypt/replay failure on an established session **closes** it (and notifies
  the peer), rather than continuing in an unsafe state.
- On reconnect the client **drops every session** — frames may have been lost
  during the gap, and a session must never span a gap. The next message
  re-handshakes.

### What's deliberately absent

- **No message ratchet.** Forward secrecy is per *session*, not per message:
  compromising a live session key exposes that session's traffic, but not past
  sessions (dh1 uses fresh ephemerals each time).
- **No offline delivery.** Both parties must be online; `SendDirect` to an
  offline peer fails (closed) rather than queueing plaintext anywhere.
- **No injectable randomness.** Entropy is `crypto/rand` only.

### Identity verification (fingerprints)

`Fingerprint` renders the first 16 bytes of `SHA-256(publicKey)` as grouped hex
(e.g. `a1b2 c3d4 …`). Both human clients show your own and your peer's
fingerprint. For high-stakes conversations, compare them out-of-band — that is
the only defense against a first-contact MITM (see below).

---

## Threat model

**Audience: everyone.**

### Protected against

- A passive or active **network attacker** — TLS 1.3 with mandatory cert
  verification.
- A **curious or compromised server** reading DM contents (it only relays
  opaque envelopes) or impersonating a DM peer **to an already-pinned user**
  (the pin mismatch fails closed).
- **Credential theft via the database** — only argon2id hashes and SHA-256
  token hashes are stored.
- **Sender spoofing** — sender fields are server-assigned from the
  authenticated identity.
- **DM replay** — strictly increasing counters bound into the AEAD AAD.

### Not protected against

- **First-contact MITM.** TOFU pins whatever key is seen first. A malicious
  server can substitute its own key on the *very first* handshake between two
  users. This is exactly why fingerprints exist and must be compared
  out-of-band for sensitive conversations.
- **Traffic analysis.** The server sees DM *metadata* — who talks to whom, and
  when — even though it can't read content.
- **Group messages.** They are stored server-side and readable by the server.
  Only 1:1 DMs are end-to-end encrypted.
- **Per-message compromise window.** No ratchet means forward secrecy is
  per-session, not per-message.

---

## Operator notes

- **You cannot read DMs, and you can't recover them.** They are never written
  to disk. A database compromise yields group history and metadata, not DM
  content. Backups never contain DMs.
- **TOFU mismatches and certificate rotation are linked.** Clients scope
  identity keys and pins to the server's TLS **public-key** fingerprint. A
  renewal that *reuses the key* is transparent; a *new key* resets every
  client's identity/pin state and forces DM peers to re-pin. See
  [operations.md](operations.md#tls-certificates).
- **A user hitting "identity key … changed — verify out-of-band"** is the
  fail-closed TOFU guard. Legitimate causes: the peer logged in on a fresh
  machine (new identity key) or after losing their local state. The user
  resolves it by verifying the new fingerprint out-of-band and removing that
  peer's entry from their local `pins.json` to re-pin. The server operator
  cannot and should not "fix" this server-side.
- **Debug logging exposes metadata.** `--log-level debug` logs envelope
  type, sender, recipient, and base64 **ciphertext**. Plaintext is never
  logged, but don't run debug against real traffic. See
  [operations.md](operations.md#flags).
