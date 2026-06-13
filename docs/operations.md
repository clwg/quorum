# Operations & deployment

**Audience:** operators running a `quorum-server`. For how the pieces fit
together see [architecture.md](architecture.md); for the DM threat model see
the "Operator notes" in [e2ee.md](e2ee.md).

> **Maturity warning.** quorum ships a *development* certificate generator and
> has not been hardened for hostile production exposure. Treat the guidance
> below as the baseline for a self-hosted/trusted-network deployment, and read
> the [Security posture](#security-posture) and [e2ee.md](e2ee.md) threat model
> before exposing it to the public internet.

## The server binary

`quorum-server` is a single static Go binary. Build it with `make quorum-server`
(output `bin/quorum-server`) or `make install`.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--listen` | `:8443` | TCP listen address. |
| `--cert` | `certs/server.pem` | TLS certificate (PEM). |
| `--key` | `certs/server-key.pem` | TLS private key (PEM). |
| `--db` | `quorum.db` | SQLite database path (created if absent). |
| `--init-admin <name>` | `""` | Create an admin user with this name, prompt for a password, then **exit** without serving. |
| `--log-level` | `info` | `debug` \| `info` \| `warn` \| `error`. |

Logs are structured (slog text handler) to **stderr**.

> `--log-level debug` logs DM relay metadata *including the base64 ciphertext
> payload* of every `DirectEnvelope`. Plaintext is never logged (the server
> can't read it), but ciphertext + sender/recipient/timestamp is sensitive
> metadata. Do not run `debug` against real traffic.

## First-run sequence

```sh
# 1. Generate a server certificate (dev CA shown here; see TLS section).
make certs                       # → certs/{ca.pem,server.pem,server-key.pem}

# 2. Bootstrap the first admin (interactive password prompt, min 8 chars).
quorum-server --db quorum.db --init-admin adminuser

# 3. Start serving.
quorum-server --listen :8443 \
  --cert certs/server.pem --key certs/server-key.pem --db quorum.db
```

`--init-admin` is the only way to create the *first* admin. After that, use the
admin TUI (`quorum-admin`) to create more users, admins, and bots. If you pipe
the password in (non-interactive), the server reads one line from stdin - useful
for scripted bring-up, but the password may land in your shell history.

There is intentionally **no open registration**: every account is created by an
admin.

## TLS certificates

All traffic is gRPC over TLS, and the server **requires TLS 1.3**. Clients
verify the server certificate against a CA they're given with `--ca` (there is
no insecure-skip path), and they pin the server's public-key fingerprint, which
scopes all their local state (identity keys, TOFU pins).

### Development

`make certs` runs [`quorum-gencert`](../cmd/quorum-gencert/main.go), producing a
throwaway P-256 CA and a server cert valid one year with SANs `localhost`,
`127.0.0.1`, `::1`. **This is for local use only** - the CA private key is
written right next to everything else and the SANs only cover loopback.

### Production

Use a certificate from your own CA or a public one (e.g. ACME/Let's Encrypt),
with SANs matching the hostname clients dial. Point `--cert`/`--key` at it.
Operational notes:

- **Key file permissions:** keep `server-key.pem` at mode `0600`, owned by the
  server's user.
- **Rotation has a cost.** Clients scope their identity keys and TOFU pins to
  the server's *public-key* fingerprint. If a cert renewal **reuses the same
  key** (same SPKI), the fingerprint is unchanged and clients are unaffected.
  If you generate a **new key**, the fingerprint changes and every client is
  treated as talking to a brand-new server: fresh identity keys, empty pin
  store, and existing DM peers will re-pin (their next DM handshake re-runs
  TOFU). Prefer key continuity across renewals unless you intend that reset.
- If clients trust a public root, they can omit `--ca`.

## The database

One SQLite file (path from `--db`). The driver is `modernc.org/sqlite` (pure
Go, no cgo). The DSN enables **WAL**, **foreign keys**, and a 5s busy timeout,
and the connection pool is capped at one connection (SQLite has a single
writer). Schema and tables are described in
[architecture.md](architecture.md#persistence).

### Backups

Because WAL is on, the database is the file **plus** its `-wal` and `-shm`
sidecars. Two safe options:

- **Hot, consistent:** `sqlite3 quorum.db ".backup '/path/backup.db'"` (uses
  SQLite's online backup API - safe while the server runs).
- **Cold:** stop the server, then copy `quorum.db*` (all three files).

A plain `cp quorum.db` of a running server can miss data still in the WAL -
prefer `.backup`.

### What's in there (and what isn't)

Stored: users, **argon2id password hashes** (never plaintext), session token
**hashes** (never the tokens), the X25519 public-key directory, channels and
membership, **group** message history, and bot token **hashes**.

Not stored: any password or token in clear, and **any 1:1 DM** - direct
messages are relayed in real time and never written to disk (see
[e2ee.md](e2ee.md)). A database compromise therefore yields group history and
metadata, but neither credentials in usable form nor DM contents.

## Sessions, tokens, and lifecycle

- **User sessions** (`qsess_…`) last 7 days as a *sliding* window: activity
  extends expiry (the server writes the extension at most once a minute per
  token). A background sweep deletes expired sessions hourly.
- **Logout** invalidates *all* of a user's sessions.
- **Admin actions kick live connections.** Disabling a user, deleting a user,
  resetting a password, or rotating a bot token immediately drops that user's
  streaming connection and (for the first three) deletes their sessions.
- **Bot tokens** (`qbot_…`) don't expire; they're revoked by rotation
  (`RevokeBotToken`), which mints a new token shown once and kicks the old
  connection. See [bot-sdk.md](bot-sdk.md#provisioning-operator).

## Rate limits

Two token-bucket limiters are built in
([`internal/server/ratelimit.go`](../internal/server/ratelimit.go)):

| What | Key | Sustained | Burst |
| --- | --- | --- | --- |
| `Login` attempts | username | ~5 / min | 5 |
| `SendDirect` (DM relay) | sender user ID | 20 / sec | 40 |

Channel messages, channel ops, and admin RPCs are **not** rate-limited at the
application layer. If you expose the server publicly, put it behind your own
ingress/L4 protections as well.

## Connection keepalive

The server sends gRPC keepalive pings every 30s (10s timeout) and enforces a
client minimum ping interval of 15s (`PermitWithoutStream: true`). The bundled
clients comply; if you write your own gRPC client, don't ping more often than
every 15s or the server will drop you for keepalive abuse.

## Process management

The server handles `SIGINT`/`SIGTERM` with a graceful gRPC stop (in-flight RPCs
drain). For an init system, a minimal unit needs only the binary, the flags
above, a working directory containing `certs/` and the DB (or absolute paths),
and a restart policy. There is no clustering: run **one** server per database -
presence and the realtime hub are in-process memory
([architecture.md](architecture.md#the-hub-fan-out-and-presence)), so a second
process pointed at the same DB would not share live state and SQLite's single
writer makes concurrent writers unsafe.

## Security posture

A consolidated view for operators (full DM analysis in [e2ee.md](e2ee.md)):

- **Transport:** TLS 1.3 only, mandatory cert verification, no skip path.
- **Credentials:** argon2id (RFC 9106 params) password hashes; only SHA-256
  token hashes stored; constant-time-ish login regardless of user existence.
- **Authorization:** every non-login RPC is token-gated; AdminService is
  role-gated in the interceptor; sender fields are server-assigned.
- **DM confidentiality:** end-to-end; the server relays opaque envelopes and
  cannot read them.
- **Known limits:** group messages are server-readable and persisted; the
  server sees DM *metadata* (who/when); first-contact DM is TOFU (a malicious
  server can MITM the very first handshake - hence the fingerprints clients
  show); no per-message ratchet. DMs need both parties online.

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| Client: `server fingerprint: …` / TLS verify error on dial | Wrong/missing `--ca`, or cert SAN doesn't match the dialed host. |
| Client: `invalid credentials` | Wrong password, disabled account, or a bot trying to `Login` (bots use tokens). |
| Client: `too many login attempts` | Login rate limit (~5/min per username); wait. |
| DM: `user offline - direct messages require both parties online` | Recipient has no live stream. DMs are online-only by design. |
| DM: `identity key for X changed … verify out-of-band` | TOFU pin mismatch - key rotation or MITM. See [e2ee.md](e2ee.md#operator-notes). |
| Client kicked / `stream replaced or terminated` | A newer login for the same user took over, or an admin action kicked you, or your buffer overflowed (laggy link). The client auto-reconnects. |
| Bot: `token rejected` | Bad/rotated `qbot_` token; reissue in the admin TUI. |
| Server won't start: migrate error | DB file from an incompatible/newer schema, or corruption - restore a backup. |
