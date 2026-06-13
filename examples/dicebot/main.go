// Command dicebot is an example quorum bot: /roll NdS rolls dice.
//
// Usage:
//
//	export QUORUM_BOT_TOKEN=qbot_...   # from the admin TUI ([2] Bots → a)
//	go run ./examples/dicebot --addr localhost:8443 --ca certs/ca.pem --channel general
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"

	"github.com/layer8/quorum/sdk/bot"
)

func main() {
	addr := flag.String("addr", "localhost:8443", "server address")
	caFile := flag.String("ca", "", "CA certificate (default: system roots)")
	channel := flag.String("channel", "general", "channel to join")
	flag.Parse()

	token := os.Getenv("QUORUM_BOT_TOKEN")
	if token == "" {
		log.Fatal("set QUORUM_BOT_TOKEN (create a bot in the admin TUI)")
	}

	var opts []bot.Option
	if *caFile != "" {
		opts = append(opts, bot.WithCAFile(*caFile))
	}
	b, err := bot.New(*addr, token, opts...)
	if err != nil {
		log.Fatal(err)
	}
	defer b.Close()

	b.Command("roll", "roll dice, e.g. /roll 2d6", func(ctx context.Context, c *bot.Command) error {
		n, sides, err := parseDice(c.RawArgs)
		if err != nil {
			return c.Replyf("%s — usage: /roll NdS (e.g. 2d6)", err)
		}
		total := 0
		for range n {
			total += rand.IntN(sides) + 1
		}
		return c.Replyf("%s rolled %d 🎲 (%dd%d)", c.Sender.Username, total, n, sides)
	})

	ctx := context.Background()
	if err := b.JoinChannel(ctx, *channel); err != nil {
		log.Fatal("join channel:", err)
	}
	log.Printf("dicebot up in #%s", *channel)
	log.Fatal(b.Run(ctx))
}

// parseDice parses "NdS" (both parts optional: "" -> 1d6, "3" -> 3d6).
func parseDice(s string) (n, sides int, err error) {
	n, sides = 1, 6
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return n, sides, nil
	}
	numStr, sideStr, hasD := strings.Cut(s, "d")
	if numStr != "" {
		if n, err = strconv.Atoi(numStr); err != nil || n < 1 || n > 100 {
			return 0, 0, fmt.Errorf("bad dice count %q", numStr)
		}
	}
	if hasD && sideStr != "" {
		if sides, err = strconv.Atoi(sideStr); err != nil || sides < 2 || sides > 1000 {
			return 0, 0, fmt.Errorf("bad die size %q", sideStr)
		}
	}
	return n, sides, nil
}
