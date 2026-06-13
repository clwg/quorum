// Package bot is the quorum bot SDK. Bots are ordinary chat clients that
// authenticate with a bot token (created in the admin TUI), join channels,
// and respond to slash commands.
//
// Minimal bot:
//
//	b, err := bot.New("chat.example.com:8443", os.Getenv("QUORUM_BOT_TOKEN"))
//	b.Command("ping", "replies with pong", func(ctx context.Context, c *bot.Command) error {
//	    return c.Reply("pong")
//	})
//	b.JoinChannel(ctx, "general")
//	log.Fatal(b.Run(ctx))
//
// Command routing is client-side: the bot sees every message in channels
// it has joined, and the SDK dispatches messages of the form "/name args"
// to the matching registered handler. Commands are also registered with
// the server so its built-in /commands listing can advertise them.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	quorumv1 "github.com/clwg/quorum/gen/quorum/v1"
	"github.com/clwg/quorum/internal/client"
)

// User identifies a message sender.
type User struct {
	ID       string
	Username string
}

// Message is a channel message as seen by the bot.
type Message struct {
	ChannelID string
	Sender    User
	Text      string

	bot *Bot
}

// Reply sends text to the channel the message arrived in.
func (m *Message) Reply(text string) error {
	return m.bot.Send(context.Background(), m.ChannelID, text)
}

// Command is a parsed slash command addressed to one of the bot's
// registered handlers.
type Command struct {
	Name      string   // without the leading slash
	RawArgs   string   // everything after the command word
	Args      []string // RawArgs split on whitespace
	ChannelID string
	Sender    User

	bot *Bot
}

// Reply sends text to the channel the command was issued in.
func (c *Command) Reply(text string) error {
	return c.bot.Send(context.Background(), c.ChannelID, text)
}

// Replyf is Reply with formatting.
func (c *Command) Replyf(format string, a ...any) error {
	return c.Reply(fmt.Sprintf(format, a...))
}

// HandlerFunc handles one invocation of a registered command.
type HandlerFunc func(ctx context.Context, cmd *Command) error

type Option func(*options)

type options struct {
	caFile  string
	dataDir string
	logger  *slog.Logger
}

// WithCAFile points the bot at a private CA (e.g. the dev CA from
// quorum-gencert). Without it, system roots are used.
func WithCAFile(path string) Option { return func(o *options) { o.caFile = path } }

// WithLogger replaces the default slog logger.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithDataDir overrides the local state directory.
func WithDataDir(dir string) Option { return func(o *options) { o.dataDir = dir } }

type command struct {
	help    string
	handler HandlerFunc
}

// Bot is a connected quorum bot.
type Bot struct {
	c      *client.Client
	token  string
	logger *slog.Logger

	mu        sync.Mutex
	commands  map[string]command
	onMessage func(ctx context.Context, m *Message)
	userID    string
}

// New dials the server and prepares a bot authenticated by token. Call
// Command/OnMessage/JoinChannel, then Run.
func New(serverAddr, token string, opts ...Option) (*Bot, error) {
	if !strings.HasPrefix(token, "qbot_") {
		return nil, errors.New("bot: token must be a qbot_ token (create one in the admin TUI)")
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	c, err := client.Dial(client.Config{Addr: serverAddr, CAFile: o.caFile, DataDir: o.dataDir})
	if err != nil {
		return nil, err
	}
	c.SetToken(token)
	return &Bot{c: c, token: token, logger: o.logger, commands: make(map[string]command)}, nil
}

// Command registers a slash-command handler. name is matched without the
// leading slash, case-insensitively.
func (b *Bot) Command(name, help string, h HandlerFunc) {
	name = strings.ToLower(strings.TrimPrefix(name, "/"))
	b.mu.Lock()
	b.commands[name] = command{help: help, handler: h}
	b.mu.Unlock()
}

// OnMessage registers a handler for every channel message the bot sees
// (including ones that are not commands). Optional.
func (b *Bot) OnMessage(h func(ctx context.Context, m *Message)) {
	b.mu.Lock()
	b.onMessage = h
	b.mu.Unlock()
}

// JoinChannel joins (creating if necessary) the named channel.
func (b *Bot) JoinChannel(ctx context.Context, name string) error {
	name = strings.TrimPrefix(name, "#")
	channels, err := b.c.ListChannels(ctx)
	if err != nil {
		return err
	}
	for _, ch := range channels {
		if strings.EqualFold(ch.GetName(), name) {
			_, err := b.c.JoinChannel(ctx, ch.GetId())
			return err
		}
	}
	_, err = b.c.CreateChannel(ctx, name)
	return err
}

// Send posts text to a channel by ID.
func (b *Bot) Send(ctx context.Context, channelID, text string) error {
	return b.c.SendChannelMessage(ctx, channelID, text)
}

// Run connects the event stream and dispatches handlers until ctx ends.
// It re-registers commands after every (re)connect and recovers from
// handler panics.
func (b *Bot) Run(ctx context.Context) error {
	if _, _, _, err := b.c.WhoAmI(ctx); err != nil {
		return fmt.Errorf("bot: token rejected: %w", err)
	}
	b.c.Run(ctx, func(ev client.Event) {
		switch ev := ev.(type) {
		case client.ConnStateEvent:
			b.logger.Info("connection state", "state", ev.State.String(), "err", ev.Err)
		case client.ResyncEvent:
			go b.registerCommands(ctx)
		case client.ChannelMessageEvent:
			go b.handleMessage(ctx, ev)
		}
	})
	return ctx.Err()
}

func (b *Bot) registerCommands(ctx context.Context) {
	b.mu.Lock()
	specs := make([]*quorumv1.CommandSpec, 0, len(b.commands))
	for name, c := range b.commands {
		specs = append(specs, &quorumv1.CommandSpec{Name: name, Help: c.help})
	}
	b.mu.Unlock()
	if len(specs) == 0 {
		return
	}
	dupes, err := b.c.RegisterCommands(ctx, specs)
	if err != nil {
		b.logger.Warn("command registration failed", "err", err)
		return
	}
	if len(dupes) > 0 {
		b.logger.Warn("commands already claimed by another bot", "names", dupes)
	}
}

func (b *Bot) handleMessage(ctx context.Context, ev client.ChannelMessageEvent) {
	msg := ev.Msg
	// Never react to our own messages (loop protection).
	if msg.GetSenderId() == b.c.UserID() {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			b.logger.Error("handler panic", "recover", r)
		}
	}()

	text := msg.GetBody()
	sender := User{ID: msg.GetSenderId(), Username: msg.GetSenderName()}

	b.mu.Lock()
	onMessage := b.onMessage
	b.mu.Unlock()
	if onMessage != nil {
		onMessage(ctx, &Message{ChannelID: msg.GetChannelId(), Sender: sender, Text: text, bot: b})
	}

	if !strings.HasPrefix(text, "/") {
		return
	}
	fields := strings.Fields(text)
	name := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	b.mu.Lock()
	cmd, ok := b.commands[name]
	b.mu.Unlock()
	if !ok {
		return
	}
	rawArgs := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	c := &Command{
		Name:      name,
		RawArgs:   rawArgs,
		Args:      fields[1:],
		ChannelID: msg.GetChannelId(),
		Sender:    sender,
		bot:       b,
	}
	if err := cmd.handler(ctx, c); err != nil {
		b.logger.Error("command failed", "command", name, "err", err)
		_ = c.Reply("⚠ command failed: " + err.Error())
	}
}

// Close tears down the connection.
func (b *Bot) Close() error { return b.c.Close() }
