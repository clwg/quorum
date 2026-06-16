// Command claudebot is an example quorum bot: /claude <query> runs the Claude
// CLI (claude -p "<query>") and posts the answer back to the channel.
//
// The machine running the bot must have the `claude` CLI installed and
// authenticated (it inherits this process's environment, e.g. ANTHROPIC_API_KEY
// or a prior `claude login`).
//
// Usage:
//
//	export QUORUM_BOT_TOKEN=qbot_...   # from the admin TUI ([2] Bots → a)
//	go run ./examples/claudebot --addr localhost:8443 --ca certs/ca.pem --channel general
//	# in a chat client:  /claude explain goroutines in one sentence
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/clwg/quorum/sdk/bot"
)

// maxChunk caps each reply below the server's 4096-byte message limit, leaving
// headroom for the optional "[i/n] " part prefix on multi-message answers.
const maxChunk = 3900

func main() {
	addr := flag.String("addr", "localhost:8443", "server address")
	caFile := flag.String("ca", "", "CA certificate (default: system roots)")
	channel := flag.String("channel", "general", "channel to join")
	claudeBin := flag.String("claude", "claude", "path to the claude CLI")
	timeout := flag.Duration("timeout", 2*time.Minute, "max time to wait for claude")
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

	b.Command("claude", "ask Claude, e.g. /claude explain goroutines", func(ctx context.Context, c *bot.Command) error {
		query := strings.TrimSpace(c.RawArgs)
		if query == "" {
			return c.Reply("usage: /claude <query> (e.g. /claude explain goroutines)")
		}
		// claude -p can take a while; let the asker know we're on it.
		_ = c.Replyf("🤔 asking Claude for %s…", c.Sender.Username)

		answer, err := runClaude(ctx, *claudeBin, *timeout, query)
		if err != nil {
			// Reply directly (rather than returning the error) for a friendly
			// message instead of the SDK's generic "⚠ command failed".
			return c.Replyf("⚠ claude failed: %s", err)
		}
		chunks := chunk(answer, maxChunk)
		for i, part := range chunks {
			if len(chunks) > 1 {
				part = fmt.Sprintf("[%d/%d] %s", i+1, len(chunks), part)
			}
			if err := c.Reply(part); err != nil {
				return err
			}
		}
		return nil
	})

	ctx := context.Background()
	if err := b.JoinChannel(ctx, *channel); err != nil {
		log.Fatal("join channel:", err)
	}
	log.Printf("claudebot up in #%s", *channel)
	log.Fatal(b.Run(ctx))
}

// runClaude invokes `claude -p "<query>"` and returns its trimmed stdout. The
// query is passed as a single argument (no shell), so it is not interpreted.
func runClaude(ctx context.Context, bin string, timeout time.Duration, query string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-p", query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("timed out after %s", timeout)
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return "", errors.New("claude returned no output")
	}
	return out, nil
}

// chunk splits s into pieces of at most max bytes, preferring to break on a
// newline and never splitting a UTF-8 rune.
func chunk(s string, max int) []string {
	var chunks []string
	for len(s) > max {
		cut := max
		if i := strings.LastIndexByte(s[:cut], '\n'); i > max/2 {
			cut = i + 1 // keep the newline with this chunk
		} else {
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut-- // back off to a rune boundary
			}
		}
		chunks = append(chunks, s[:cut])
		s = s[cut:]
	}
	if len(s) > 0 {
		chunks = append(chunks, s)
	}
	return chunks
}
