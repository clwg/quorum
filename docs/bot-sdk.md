# Bot SDK guide

**Audience:** primarily bot **developers** using [`sdk/bot`](../sdk/bot). The
[Provisioning](#provisioning-operator) section is for **operators** who hand out
bot tokens. For where bots sit in the system, see
[architecture.md](architecture.md).

A bot is an ordinary chat client that authenticates with a **bot token** instead
of a username/password. It joins channels, sees every message in them, and
responds to slash commands. Command routing happens **client-side** in the SDK;
the server only keeps a registry of command names so its built-in `/commands`
listing can advertise them. (`/help` in the chat client is separate - it
documents the client's own built-in commands.)

> Bots operate on **group channels only**. They have no access to end-to-end
> encrypted 1:1 DMs - by design, the server can't read those, so neither can a
> bot ([e2ee.md](e2ee.md)).

## Provisioning (operator)

Tokens are minted in the admin TUI; a developer cannot create their own.

1. Run `quorum-admin` and open the **Bots** tab (`[2]`).
2. Press `a` to add a bot. A `qbot_…` token is shown **once** - copy it
   immediately; only its SHA-256 hash is stored, so it can't be retrieved later.
3. Hand the token to the bot developer over a secure channel.

Rotation/revocation: rotating a bot's token (`RevokeBotToken`) issues a new
`qbot_…` (again shown once) and immediately kicks the bot's live connection.
The old token stops working. There is no expiry; rotation is the revocation
mechanism. See [operations.md](operations.md#sessions-tokens-and-lifecycle).

## Quick start

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/clwg/quorum/sdk/bot"
)

func main() {
	b, err := bot.New("chat.example.com:8443", os.Getenv("QUORUM_BOT_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	defer b.Close()

	b.Command("ping", "replies with pong", func(ctx context.Context, c *bot.Command) error {
		return c.Reply("pong")
	})

	ctx := context.Background()
	if err := b.JoinChannel(ctx, "general"); err != nil {
		log.Fatal(err)
	}
	log.Fatal(b.Run(ctx)) // blocks until ctx is cancelled
}
```

Run it:

```sh
export QUORUM_BOT_TOKEN=qbot_...            # from the admin TUI
go run . --addr chat.example.com:8443       # if you wire up flags
```

Against a dev server using the self-signed CA, pass the CA explicitly (see
[`WithCAFile`](#options)).

## API

All types are in [`sdk/bot/bot.go`](../sdk/bot/bot.go).

### `New(serverAddr, token string, opts ...Option) (*Bot, error)`
Dials the server and prepares an authenticated bot. The token **must** start
with `qbot_`; anything else is rejected immediately. Dialing verifies TLS and
records the server fingerprint, but does not yet validate the token - that
happens in `Run`.

### Options

| Option | Effect |
| --- | --- |
| `WithCAFile(path)` | Trust a private CA (e.g. the dev CA from `quorum-gencert`). Without it, system roots are used. |
| `WithLogger(*slog.Logger)` | Replace the default slog logger. |
| `WithDataDir(dir)` | Override the local state directory (default `~/.config/quorum`). |

### `(*Bot) Command(name, help string, h HandlerFunc)`
Register a slash-command handler. `name` is matched without the leading slash,
case-insensitively. `help` shows up in the `/commands` listing. The handler:

```go
type HandlerFunc func(ctx context.Context, cmd *bot.Command) error
```

`*bot.Command` gives you `Name`, `RawArgs` (everything after the command word),
`Args` (`RawArgs` split on whitespace), `ChannelID`, and `Sender` (`{ID,
Username}`). Reply with `c.Reply(text)` or `c.Replyf(format, …)` - both post to
the channel the command came from. If a handler returns an error, the SDK logs
it and replies `⚠ command failed: <err>` in-channel.

### `(*Bot) OnMessage(func(ctx, *bot.Message))`
Optional. Fires for **every** channel message the bot sees, command or not.
Useful for keyword triggers or logging. `*bot.Message` has `ChannelID`,
`Sender`, `Text`, and its own `Reply`.

### `(*Bot) JoinChannel(ctx, name) error`
Joins the named channel, **creating it if it doesn't exist** (a leading `#` is
stripped). Call before `Run`, or any time after.

### `(*Bot) Send(ctx, channelID, text) error`
Post to a channel by ID (when you're not replying to a specific message).

### `(*Bot) Run(ctx) error`
The main loop. It first calls `WhoAmI` to validate the token (returning
`bot: token rejected` on failure), then pumps the event stream and dispatches
handlers until `ctx` is cancelled. It blocks. Cancel the context (or signal) to
stop.

### `(*Bot) Close() error`
Tear down the connection.

## Behavior you get for free

- **Loop protection.** The bot never reacts to its own messages.
- **Panic recovery.** A panic in a handler is recovered and logged; it won't
  crash the bot.
- **Reconnect + re-registration.** The underlying client reconnects with
  backoff. After every (re)connect the SDK **re-registers** the bot's commands
  (`ResyncEvent`), so `/commands` stays correct across reconnects.
- **Duplicate-command warnings.** If another bot already claimed a command
  name, `RegisterCommands` returns the dupes and the SDK logs a warning. The
  duplicate simply isn't owned by you - there's no hard error.

## Concurrency

`OnMessage` and command handlers run in their **own goroutines** (the SDK
`go`-dispatches each message). If your handlers touch shared state, guard it.
Replies are independent RPCs and safe to call concurrently.

## Command routing, precisely

1. The bot receives a `ChannelMessage` for a channel it has joined.
2. `OnMessage` (if set) is called for every such message.
3. If the text starts with `/`, the first whitespace-delimited token (minus the
   slash, lowercased) is looked up in the registered commands. A match invokes
   the handler; a non-match is ignored.

Routing is entirely client-side. `RegisterCommands` exists only so the server's
built-in `/commands` listing can advertise your commands and warn about name
collisions - it does **not** affect dispatch.

## Worked example: dicebot

[`examples/dicebot/main.go`](../examples/dicebot/main.go) is a complete,
runnable bot: `/roll NdS` rolls dice (`/roll 2d6`, defaults to `1d6`). It shows
the whole shape - flag parsing, `WithCAFile`, one `Command` with argument
parsing and `Replyf`, `JoinChannel`, and `Run`.

```sh
# create a bot in the admin TUI, then:
export QUORUM_BOT_TOKEN=qbot_...
go run ./examples/dicebot --addr localhost:8443 --ca certs/ca.pem --channel general
# in a chat client:  /roll 2d6
```

Note how it validates arguments and replies with a usage hint on bad input
(`return c.Replyf("%s - usage: /roll NdS", err)`) rather than returning an
error - returning an error would surface the generic `⚠ command failed` reply
instead of a friendly message.
