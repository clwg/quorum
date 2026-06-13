# quorum

A multi-user chat system in Go: a gRPC server with encrypted, authenticated
sessions; group channels with persisted history; end-to-end-encrypted 1:1
direct messages; a GUI client; a terminal client; an admin terminal UI; and a bot SDK.

## Components

- **cmd/quorum-server** - headless CLI server. Flags: `--listen`, `--cert`,
  `--key`, `--db`, `--init-admin <name>`, `--log-level`.
- **cmd/quorum-client** - bubbletea chat TUI. Sidebar of channels and DMs, a
  message viewport, an input line, a status bar with connection state and
  E2EE key fingerprints. Slash commands: `/create`, `/join`, `/leave`,
  `/dm <user>`, `/passwd` (change your password), `/commands` (list bot
  commands), `/help`, `/quit`.
- **cmd/quorum-gui** - Fyne desktop chat client; a graphical peer of
  quorum-client driving the same `internal/client` core.
- **cmd/quorum-admin** - bubbletea admin TUI over the role-gated AdminService:
  add/disable/delete users, reset passwords, create/rotate/delete bots.
- **sdk/bot** - Go SDK for writing bots; **examples/dicebot** is a worked
  example.
- **cmd/quorum-gencert** - generates a dev CA and server certificate. Not for
  production.

## Quickstart

This section walks you through cloning, building, and running Quorum.

### Prerequisites

The GUI client is built with the [Fyne](https://fyne.io) framework, which
requires a C compiler and a system graphics driver. See the
[Fyne Getting Started guide](https://docs.fyne.io/started/quick/) for
platform-specific install instructions.

### Clone the repository

```bash
git clone git@github.com:clwg/quorum.git
cd quorum
```

### Build the application

Build every component at once:

```bash
make build
```

On MacOS the following warning can be safely ignored ```ld: warning: ignoring duplicate libraries: '-lobjc'```

Or build components individually:

| Component       | Command               | Output binary        |
| --------------- | --------------------- | -------------------- |
| Server          | `make quorum-server`  | `./bin/quorum-server` |
| Admin client    | `make quorum-admin`   | `./bin/quorum-admin`  |
| Terminal client | `make quorum-client`  | `./bin/quorum-client` |
| GUI client      | `make quorum-gui`     | `./bin/quorum-gui`    |

### Generate certificates

Quorum uses TLS. Generate a CA and server certificate into the `certs/`
directory:

```bash
make certs
```

### Running the application

#### 1. Initialize the server and create the admin user

The first time you run the server, pass `--init-admin <username>` to create the
database and the initial admin user. You will be prompted to set a password.

```bash
./bin/quorum-server \
  --listen :8443 \
  --cert certs/server.pem \
  --key certs/server-key.pem \
  --db quorum.db \
  --init-admin admin
```

#### 2. Start the server

Once the admin user exists, start the server normally:

```bash
./bin/quorum-server \
  --listen :8443 \
  --cert certs/server.pem \
  --key certs/server-key.pem \
  --db quorum.db
```

#### 3. Launch the admin client

The admin client is used for managing users and bots. Log in with your admin
credentials, then create new users and bots.

```bash
./bin/quorum-admin --addr localhost:8443 --ca certs/ca.pem
```

#### 4. Launch the terminal (TUI) client

Pass the server address and CA certificate, then provide valid credentials when
prompted.

```bash
./bin/quorum-client --addr localhost:8443 --ca certs/ca.pem
```

#### 5. Launch the GUI client

The GUI client can be started without arguments and configured from within the
interface (you will need to provide the path to `certs/ca.pem`):

```bash
./bin/quorum-gui
```

You can also pass `--addr` and `--ca` to pre-fill the connection defaults:

```bash
./bin/quorum-gui --addr localhost:8443 --ca certs/ca.pem
```

#### Bot Example

```sh
# Create a bot in quorum-admin: tab [2] Bots → 'a' → copy the qbot_ token that is shown
export QUORUM_BOT_TOKEN=qbot_...
go run ./examples/dicebot --ca certs/ca.pem --channel general
# then in a client: /roll 2d6
```


## Documentation

Additional docs live in [docs/](docs/):

- [docs/architecture.md](docs/architecture.md) - components, the gRPC services,
  the hub, data flow, and the SQLite schema (for contributors).
- [docs/operations.md](docs/operations.md) - running a server: flags, TLS,
  backups, rate limits, hardening, troubleshooting (for operators).
- [docs/e2ee.md](docs/e2ee.md) - the DM encryption protocol spec and threat
  model.
- [docs/bot-sdk.md](docs/bot-sdk.md) - writing bots with `sdk/bot`.

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
between two users (TOFU pins the attacker's key) - this is why fingerprints
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
