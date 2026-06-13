// Command quorum-admin is the management TUI for quorum servers: user and
// bot administration over the role-gated AdminService.
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/clwg/quorum/internal/client"
	"github.com/clwg/quorum/internal/tui/admin"
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

	if _, err := tea.NewProgram(admin.New(c), tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
