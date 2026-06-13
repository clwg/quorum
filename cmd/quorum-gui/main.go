// Command quorum-gui is the user-facing desktop chat client, built on Fyne.
// It is a graphical peer of the quorum-client TUI: same login, channels, DMs,
// E2EE, and slash commands, driven by the shared internal/client core.
package main

import (
	"flag"

	"github.com/clwg/quorum/internal/gui"
)

func main() {
	addr := flag.String("addr", "localhost:8443", "default server address")
	caFile := flag.String("ca", "", "CA certificate for the server (default: system roots)")
	dataDir := flag.String("data-dir", "", "client state directory (default ~/.config/quorum)")
	flag.Parse()

	gui.NewApp(gui.Defaults{
		Addr:    *addr,
		CAFile:  *caFile,
		DataDir: *dataDir,
	}).Run()
}
