package chat

import (
	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
)

// copyResultMsg reports the outcome of a clipboard write back to the Update loop
// so the status bar can confirm the copy or surface a failure.
type copyResultMsg struct{ err error }

// copyToClipboardCmd writes text to the system clipboard off the Update loop
// (the write shells out to the platform's clipboard utility). On Linux this
// needs xclip, xsel, or wl-clipboard installed; the error path reports when it
// is missing.
func copyToClipboardCmd(text string) tea.Cmd {
	return func() tea.Msg {
		return copyResultMsg{err: clipboard.WriteAll(text)}
	}
}
