// Command quorum-client is the user-facing TUI chat client.
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/clwg/quorum/internal/client"
	"github.com/clwg/quorum/internal/tui/chat"
)

func main() {
	addr := flag.String("addr", "localhost:8443", "server address")
	caFile := flag.String("ca", "", "CA certificate for the server (default: system roots)")
	dataDir := flag.String("data-dir", "", "client state directory (default ~/.config/quorum)")
	flag.Parse()

	c, err := client.Dial(client.Config{Addr: *addr, CAFile: *caFile, DataDir: *dataDir})
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer c.Close()

	model := chat.New(c)
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	model.SetSend(p.Send)

	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
