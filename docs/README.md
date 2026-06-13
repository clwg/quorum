# quorum documentation

These documents go deeper than the top-level [README](../README.md), which
stays a quick overview. Each file here is self-contained and covers one aspect
of the system. Every doc opens with an **Audience** line so you can tell at a
glance whether it's written for people *running* a server or people *reading
and extending* the code.

| Doc | Audience | What it covers |
| --- | --- | --- |
| [architecture.md](architecture.md) | Developers / contributors | Component map, the three gRPC services, the hub fan-out and presence model, end-to-end data flow, the SQLite schema, package layout. |
| [operations.md](operations.md) | Operators | Running `quorum-server`: flags, TLS certificates, bootstrapping the first admin, the database, rate limits, keepalive, backups, hardening, troubleshooting. |
| [e2ee.md](e2ee.md) | Both (split into protocol spec + operator notes) | The end-to-end encryption used for 1:1 DMs: the triple-DH handshake, transcript binding, the ChaCha20-Poly1305 frame format, TOFU pinning, and the threat model. |
| [bot-sdk.md](bot-sdk.md) | Bot developers (+ operator notes on provisioning) | Writing bots with `sdk/bot`: tokens, command routing, `OnMessage`, reconnect behavior, and a walkthrough of the dicebot example. |

## Where to start

- **I want to run a server.** Read [operations.md](operations.md), then skim
  the "Operator notes" sections of [e2ee.md](e2ee.md).
- **I want to write a bot.** Read [bot-sdk.md](bot-sdk.md).
- **I want to understand or change the code.** Read
  [architecture.md](architecture.md), then [e2ee.md](e2ee.md).

## Conventions used across these docs

- `qsess_…` is a user session token (from `Login`); `qbot_…` is a bot token
  (from the admin TUI). The server stores only their SHA-256 hashes.
- "Server identity" / "server fingerprint" means the SHA-256 of the server
  TLS certificate's SubjectPublicKeyInfo. It scopes all client-side state.
- Code references like [`internal/e2ee/session.go`](../internal/e2ee/session.go)
  are relative to the repository root.
