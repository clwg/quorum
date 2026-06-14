# Architecture

**Audience:** developers and contributors reading or extending the code.
For deployment and configuration see [operations.md](operations.md); for the
DM crypto see [e2ee.md](e2ee.md).

quorum is a single server process plus three thin clients that all share one
client core. State lives in one SQLite file. Realtime delivery is a single
server-streaming gRPC call per connected user. There is no message queue, no
cache, and no second datastore.

```
┌──────────────┐   gRPC over TLS 1.3     ┌─────────────────────────────┐
│ quorum-client│◀───────────────────────▶│         quorum-server       │
│(chat TUI/GUI)│   token in metadata     │  Auth │ Chat │ Admin svcs   │
├──────────────┤                         │  hub (fan-out + presence)   │
│ quorum-admin │◀───── AdminService ────▶│  store (SQLite: users,      │
│   (admin TUI)│   (role=admin gated)    │   channels, group history)  │
├──────────────┤                         └─────────────────────────────┘
│  bots (SDK)  │◀───── qbot_ token ─────▶   1:1 DMs relayed opaquely;
└──────────────┘                            never stored, never readable
```

## Components

### Server (`cmd/quorum-server`)
A headless binary. It opens the store, loads the TLS keypair, builds the
auth interceptors and the hub, registers the three services, and serves gRPC.
A background goroutine sweeps expired sessions hourly. `--init-admin <name>`
is a one-shot mode: it creates an admin user and exits without serving. See
[`cmd/quorum-server/main.go`](../cmd/quorum-server/main.go).

### Clients
All three are built on the same [`internal/client`](../internal/client)
package, which owns dialing, login/token auth, the Subscribe pump with
reconnect, and (for the two human clients) the E2EE session manager.

- **`cmd/quorum-client`** - the chat TUI ([`internal/tui/chat`](../internal/tui/chat)),
  a bubbletea app: channel/DM sidebar, message viewport, input line, status
  bar with connection state and E2EE fingerprints. Slash commands are parsed
  client-side (`/create`, `/join`, `/leave`, `/dm`, `/search`, `/passwd`,
  `/commands`, `/help`, `/quit`). `/help` is rendered locally; `/search` calls
  `ChatService.SearchChannelMessages` and shows the matches in a dedicated
  results overlay; `/passwd` opens a modal form that calls
  `AuthService.ChangePassword`; `/commands` asks the server for the bot commands
  registered in this channel.
- **`cmd/quorum-admin`** - the admin TUI ([`internal/tui/admin`](../internal/tui/admin)),
  driving `AdminService`.
- **`sdk/bot` + `examples/dicebot`** - bots are ordinary token-authenticated
  clients. See [bot-sdk.md](bot-sdk.md).
- **`cmd/quorum-gencert`** - dev-only CA + server cert generator.

## The three gRPC services

Service definitions live in [`proto/quorum/v1`](../proto/quorum/v1); the
committed generated code is in [`gen/quorum/v1`](../gen/quorum/v1). Implementations
are in [`internal/server`](../internal/server).

### AuthService ([`authsvc.go`](../internal/server/authsvc.go))
Login/logout and the identity-key directory. `Login` is the **only** RPC that
does not require a bearer token. Notable details:

- `Login` is rate-limited per username (~5/min, burst 5) and runs a dummy
  argon2 verification when the user doesn't exist, so present and absent
  usernames take comparable time. Bots cannot log in (they have no password).
- `ChangePassword` is the self-service counterpart to the admin-only
  `AdminService.ResetPassword`: the caller proves ownership with their current
  password and sets a new one. Unlike the admin reset (which kills the target's
  sessions), it leaves existing sessions intact, so the user is not logged out
  of the client they changed it from. Bots are rejected (they have tokens, not
  passwords).
- `WhoAmI` resolves identity from the bearer token; bots call this instead of
  `Login`.
- `PublishIdentityKey` / `GetIdentityKey` are the X25519 directory used to
  bootstrap DM sessions (see [e2ee.md](e2ee.md)). Keys are 32 bytes; the
  directory returns an empty key for users who have never logged in.

### ChatService ([`chatsvc.go`](../internal/server/chatsvc.go))
The realtime core. One server-streaming `Subscribe` delivers every event to a
client; the rest are unary.

- **`Subscribe`** registers a hub subscriber and streams `ServerEvent`s. A
  newer `Subscribe` for the same user kicks the older stream.
- **Channel ops** (`SendChannelMessage`, `Create/Join/Leave/ListChannels`,
  `GetChannelHistory`, `SearchChannelMessages`, `ListUsers`) check membership,
  persist where relevant, and fan out events. Sender identity is always taken
  from the authenticated context, never from the request - clients cannot spoof
  a sender. `SearchChannelMessages` runs a case-insensitive `LIKE` substring
  match scoped to one channel, returning the most recent matches.
- **`SendDirect`** relays one opaque `DirectEnvelope` to a connected recipient
  and persists nothing. It overwrites the sender fields, enforces a payload
  size cap, and is rate-limited (20/s/sender, burst 40). Offline recipient ⇒
  `Unavailable` (fail closed). The payload is never inspected.
- **`RegisterCommands`** lets a bot declare its slash commands for `/commands`
  discovery only; command *routing* is entirely client-side.

### AdminService ([`adminsvc.go`](../internal/server/adminsvc.go))
User and bot lifecycle, reachable only by `role=admin` (enforced in the
interceptor, not per-handler). Create/disable/delete users, reset passwords,
create/rotate bot tokens. Disabling, deleting, resetting a password, or
rotating a bot token all `Kick` the affected user's live stream. Admins cannot
disable or delete their own account (`guardSelf`).

## Authentication and the interceptor

[`internal/auth`](../internal/auth) holds password hashing, token minting, and
the gRPC interceptors. Every non-exempt RPC passes through
[`interceptor.go`](../internal/auth/interceptor.go):

1. Pull the `authorization: Bearer <token>` metadata.
2. If the token has the `qbot_` prefix, resolve it as a bot token hash;
   otherwise resolve it as a session token hash and check expiry.
3. Reject disabled accounts.
4. For `qsess_` tokens, lazily extend session expiry - at most once per minute
   per token, to avoid a DB write on every RPC.
5. For `/quorum.v1.AdminService/*`, require `role=admin`.
6. Inject the resolved `*auth.Identity` into the request context; handlers read
   it with `auth.FromContext`.

Only token **hashes** (SHA-256) are stored. Passwords are argon2id in PHC
format ([`password.go`](../internal/auth/password.go)); parameters are parsed
from the stored string, so they can be raised over time without breaking old
hashes.

## The hub: fan-out and presence

[`internal/hub`](../internal/hub) is an in-memory map of `userID → Subscriber`
with a buffered channel per subscriber. It is the single source of truth for
presence - nothing about who's online touches the database.

- **One stream per user.** `Register` kicks any previous subscriber for the
  same user (closing its `Done` channel).
- **Non-blocking delivery.** `SendToUser`/`FanOut`/`Broadcast` never block: if a
  subscriber's buffer (256 events) is full, the hub closes it. The client
  reconnects and resyncs from scratch rather than the server stalling.
- **Presence** is derived from map membership; `Subscribe` broadcasts an
  online presence event on connect and an offline one on disconnect (deferred).

This is why presence and DMs are inherently online-only and single-process:
the hub is process-local memory. Horizontal scaling would require replacing it
with a shared bus.

## Data flow examples

**Group message.** Client → `SendChannelMessage` → membership check →
`InsertMessage` (SQLite) → `FanOut` a `ChannelMessage` to every member
*including the sender* (clients render their own messages from the stream echo,
so there is one rendering path). History is paginated by the monotonic
per-channel message `id` via `GetChannelHistory`.

**Direct message.** Sender encrypts locally and calls `SendDirect` with a
`DirectEnvelope`; the server relays it to the recipient's stream as a
`DirectEnvelope` event and stores nothing. The recipient's client decrypts and
emits a `DirectMessageEvent`. The whole handshake-and-frame protocol is in
[e2ee.md](e2ee.md); the client-side state machine is
[`internal/client/dm.go`](../internal/client/dm.go).

**Reconnect.** The client's `Run` loop ([`client.go`](../internal/client/client.go))
re-subscribes with exponential backoff (1s → 30s cap), re-logging-in with the
remembered password if the token was rejected. On every successful
(re)subscribe it **drops all E2EE sessions** (`dm.reset()`) - a session must
never continue across a gap where frames may have been lost - and emits a
`ResyncEvent` so the UI refetches channels and history.

## Persistence

[`internal/store`](../internal/store) wraps one SQLite database (driver
`modernc.org/sqlite`, pure Go, no cgo). The pool is capped at a single
connection because SQLite has one writer; WAL, foreign keys, and a 5s busy
timeout are set in the DSN. Migrations are embedded SQL files applied in
lexical order and recorded in `schema_migrations`. Schema:
[`migrations/0001_init.sql`](../internal/store/migrations/0001_init.sql).

Tables: `users`, `sessions` (token-hash keyed, with sliding expiry),
`identity_keys` (the X25519 directory), `channels`, `channel_members`,
`messages` (**group history only** - 1:1 DMs never land here), and `bots`
(owner + token hash). See [operations.md](operations.md) for backup guidance.

## Package map

| Path | Responsibility |
| --- | --- |
| [`proto/`](../proto) | gRPC service + message definitions. |
| [`gen/`](../gen) | Committed generated Go from `buf generate`. |
| [`internal/store`](../internal/store) | SQLite persistence + migrations. |
| [`internal/auth`](../internal/auth) | argon2id hashing, tokens, interceptors. |
| [`internal/hub`](../internal/hub) | Event fan-out + presence (in-memory). |
| [`internal/server`](../internal/server) | The three service implementations + rate limiter. |
| [`internal/e2ee`](../internal/e2ee) | Pure crypto core (I/O-free, unit-tested). |
| [`internal/client`](../internal/client) | Shared dial/login/pump/reconnect + DM session manager. |
| [`internal/tui/{chat,admin}`](../internal/tui) | The two bubbletea UIs. |
| [`sdk/bot`](../sdk/bot) | Public bot SDK. |
| [`examples/`](../examples) | Worked examples (dicebot). |
| [`cmd/`](../cmd) | The four binaries. |

## Design choices worth knowing

- **Single rendering path for group messages:** the sender sees its own message
  via the fan-out echo, not an optimistic local insert.
- **Fail-closed everywhere for DMs:** offline peer, handshake error, or TOFU
  mismatch drops queued plaintext; nothing is ever sent another way.
- **The crypto core is pure:** [`internal/e2ee`](../internal/e2ee) has no I/O
  and no gRPC types, with entropy only from `crypto/rand`. This keeps it
  exhaustively unit-testable and reusable across all clients.
- **Generated code is committed** so a clean checkout builds without `buf`.
  Regenerate with `make gen` after editing `proto/`.
