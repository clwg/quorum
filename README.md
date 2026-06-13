# quorum

A multi-user chat system in Go: a gRPC server with encrypted, authenticated
sessions; group channels with persisted history; end-to-end-encrypted 1:1
direct messages; a GUI client; a terminal client; an admin terminal UI; and a bot SDK.

## Quick start

```sh
brew install buf            # codegen toolchain (uses remote plugins)
make gen                    # generate gen/quorum/v1 from proto/
make certs                  # dev self-signed CA + server cert into certs/
go run ./cmd/quorum-server --db quorum.db --init-admin adminuser  # set a password
make run-server             # listens on :8443 with the dev cert

# in other terminals:
make run-client             # log in, /create general, chat, /dm <user>
make run-admin              # manage users and bots
```

Bots:

```sh
# in quorum-admin: tab [2] Bots → 'a' → copy the qbot_ token shown once
export QUORUM_BOT_TOKEN=qbot_...
go run ./examples/dicebot --ca certs/ca.pem --channel general
# then in a client: /roll 2d6
```

## Components

- **cmd/quorum-server** — headless CLI server. Flags: `--listen`, `--cert`,
  `--key`, `--db`, `--init-admin <name>`, `--log-level`.
- **cmd/quorum-client** — bubbletea chat TUI. Sidebar of channels and DMs, a
  message viewport, an input line, a status bar with connection state and
  E2EE key fingerprints. Slash commands: `/create`, `/join`, `/leave`,
  `/dm <user>`, `/commands` (list bot commands), `/help`, `/quit`.
- **cmd/quorum-gui** — Fyne desktop chat client; a graphical peer of
  quorum-client driving the same `internal/client` core. Login form, a
  channels/DMs sidebar with presence and unread badges, a word-wrapped message
  pane, E2EE fingerprints in the DM header, and the same slash commands.
- **cmd/quorum-admin** — bubbletea admin TUI over the role-gated AdminService:
  add/disable/delete users, reset passwords, create/rotate/delete bots.
- **sdk/bot** — Go SDK for writing bots; **examples/dicebot** is a worked
  example.
- **cmd/quorum-gencert** — generates a dev CA and server certificate. Not for
  production.

## Documentation

Deeper, focused docs live in [docs/](docs/):

- [docs/architecture.md](docs/architecture.md) — components, the gRPC services,
  the hub, data flow, and the SQLite schema (for contributors).
- [docs/operations.md](docs/operations.md) — running a server: flags, TLS,
  backups, rate limits, hardening, troubleshooting (for operators).
- [docs/e2ee.md](docs/e2ee.md) — the DM encryption protocol spec and threat
  model.
- [docs/bot-sdk.md](docs/bot-sdk.md) — writing bots with `sdk/bot`.

## Build

```sh
make build    # compile all commands into ./bin
make install  # install all commands into $GOBIN (or $GOPATH/bin)
```

`make build` produces `bin/quorum-server`, `bin/quorum-client`,
`bin/quorum-admin`, and `bin/quorum-gencert`. Build one with `make <name>`
(e.g. `make quorum-server`). Output dir and flags are overridable, e.g.
`make build BIN=dist` or `make build LDFLAGS=` to keep debug symbols.

## Security model

### Transport and authentication

All traffic is gRPC over TLS 1.3. Clients verify the server certificate
against a configured CA (`--ca`); there is no insecure-skip path. Users log in
with a username and password (hashed with **argon2id**, RFC 9106 parameters,
stored as PHC strings) and receive an opaque session token. The server stores
only the SHA-256 of each token. Every non-login RPC is gated by an interceptor
that resolves the token to an identity; AdminService additionally requires the
`admin` role. The server overwrites client-supplied sender fields with the
authenticated identity, so senders cannot be spoofed.

### End-to-end encryption for direct messages

1:1 messages are encrypted on the client and **relayed** by the server, which
never sees plaintext, never holds private keys, and never persists DM traffic.

- **Identity keys** are long-term X25519 keypairs, generated per server on
  first connect and stored locally (private key mode `0600`). The public half
  is published to the server's key directory.
- A conversation establishes a session with a **synchronous triple-DH**
  handshake (X25519): an ephemeral-ephemeral DH provides forward secrecy, and
  two identity-ephemeral DHs authenticate both parties. The transcript binds
  the server identity and both usernames, so a session in one server context
  can never be substituted for another.
- Messages use **ChaCha20-Poly1305** with per-direction keys and a counter
  nonce; receivers reject replayed or regressed counters.
- Identity keys are pinned **trust-on-first-use** per `(server, peer)`. A later
  key change fails closed (the handshake is blocked) until you verify the new
  key out-of-band and remove the stale pin.

**Usernames are server-local.** `alice` on one server is unrelated to `alice`
on another; identities, pins, and session transcripts are all scoped by the
server's TLS public-key fingerprint.

### What this protects against, and what it does not

Protected: a passive or active network attacker (TLS); a curious or
compromised server reading DM contents or impersonating a DM peer to an
already-pinned user; credential theft via the database (only hashes are
stored); sender spoofing; DM replay.

**Not** protected: a malicious server performing a MITM on the *first* contact
between two users (TOFU pins the attacker's key) — this is why fingerprints
are shown and must be compared out-of-band for high-stakes conversations;
traffic-analysis metadata (who talks to whom, when) is visible to the server;
group channel messages are stored server-side and readable by the server
(only 1:1 DMs are end-to-end encrypted); and there is no message ratchet, so
forward secrecy is per-session rather than per-message. Dev certificates from
`quorum-gencert` are for local use only.

DMs require both parties to be online; the server relays in real time and
stores nothing, so an offline recipient causes the send to fail (closed)
rather than queueing plaintext anywhere.

## Development

```sh
make gen      # buf lint + generate
make test     # go test ./...
make vet      # go vet ./...
```

Layout: `proto/` (service definitions) → `gen/` (committed generated code);
`internal/store` (SQLite), `internal/auth` (hashing, tokens, interceptors),
`internal/hub` (fan-out + presence), `internal/server` (service impls),
`internal/e2ee` (the crypto core, pure and unit-tested), `internal/client`
(shared dial/login/pump/reconnect used by both TUIs and the bot SDK),
`internal/tui/{chat,admin}`, `sdk/bot`, `examples/`.

The crypto core (`internal/e2ee`) is I/O-free and has exhaustive tests:
handshake agreement, tamper and replay rejection, low-order-point aborts,
context binding, and session-key uniqueness. End-to-end client tests over real
TLS cover DM round trips, fail-closed behavior for offline peers and TOFU
mismatches, simultaneous-handshake convergence, and the full bot command flow.
