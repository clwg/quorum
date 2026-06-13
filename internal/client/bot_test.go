package client_test

import (
	"context"
	"strings"
	"testing"
	"time"

	quorumv1 "github.com/layer8/quorum/gen/quorum/v1"
	"github.com/layer8/quorum/internal/client"
	"github.com/layer8/quorum/sdk/bot"
)

func TestBotEndToEnd(t *testing.T) {
	ts := startTestServer(t)
	ts.createUser(t, "alice")
	adminID := ts.createUser(t, "admin")
	admin, _, _ := ts.connect(t, "admin")
	// Promote to admin; the interceptor reads the role per-request.
	if err := ts.store.SetUserRole(context.Background(), adminID, "admin"); err != nil {
		t.Fatal(err)
	}

	// Create the bot through AdminService (returns the token once).
	ctx := context.Background()
	resp, err := admin.Admin().CreateBot(ctx, &quorumv1.CreateBotRequest{Username: "dicebot"})
	if err != nil {
		t.Fatal(err)
	}
	token := resp.GetToken()
	if !strings.HasPrefix(token, "qbot_") {
		t.Fatalf("unexpected token: %s", token)
	}

	// Alice creates #general and subscribes.
	alice, aliceEvents, _ := ts.connect(t, "alice")
	ch, err := alice.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}

	// Bot connects with the SDK, joins, and serves /ping.
	b, err := bot.New(ts.addr, token, bot.WithCAFile(ts.caFile), bot.WithDataDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.Command("ping", "replies with pong", func(ctx context.Context, c *bot.Command) error {
		return c.Replyf("pong %s", c.RawArgs)
	})
	if err := b.JoinChannel(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	botCtx, cancelBot := context.WithCancel(ctx)
	defer cancelBot()
	go b.Run(botCtx)

	// Wait for the bot's command registration to land, then /commands shows it.
	deadlineHelp := time.Now().Add(5 * time.Second)
	for {
		if err := alice.SendChannelMessage(ctx, ch.GetId(), "/commands"); err != nil {
			t.Fatal(err)
		}
		ev := waitEvent(t, aliceEvents, func(ev client.Event) bool {
			_, ok := ev.(client.SystemEvent)
			return ok
		}).(client.SystemEvent)
		if strings.Contains(ev.Notice.GetText(), "/ping") {
			break
		}
		if time.Now().After(deadlineHelp) {
			t.Fatalf("/commands never listed /ping: %q", ev.Notice.GetText())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Alice issues the command; the bot's reply comes back as a message.
	if err := alice.SendChannelMessage(ctx, ch.GetId(), "/ping hello"); err != nil {
		t.Fatal(err)
	}
	reply := waitEvent(t, aliceEvents, func(ev client.Event) bool {
		cm, ok := ev.(client.ChannelMessageEvent)
		return ok && cm.Msg.GetSenderName() == "dicebot"
	}).(client.ChannelMessageEvent)
	if reply.Msg.GetBody() != "pong hello" {
		t.Fatalf("unexpected reply: %q", reply.Msg.GetBody())
	}
}
