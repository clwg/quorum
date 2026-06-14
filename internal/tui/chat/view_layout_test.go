package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/clwg/quorum/internal/client"
)

// TestViewFitsTerminalWithLargeMessage guards the layout against long or
// contiguous message bodies. A body long enough to wrap to several full-width
// lines must not push the rendered view past the terminal's width or height: if
// it does, the side-by-side sidebar scrolls off the top and the sidebar's click
// hit-testing (sidebarHit) desyncs from what is actually drawn.
func TestViewFitsTerminalWithLargeMessage(t *testing.T) {
	sizes := []struct{ w, h int }{
		{80, 24}, {120, 40}, {60, 18}, {200, 50},
	}
	for _, sz := range sizes {
		m := New(&client.Client{})
		m.loggedIn = true
		m.Update(tea.WindowSizeMsg{Width: sz.w, Height: sz.h})
		conv := m.ensureConv(chKey("c"), "c", "#general", false)
		conv.historyLoaded = true
		m.setActive(conv.key)
		// A long contiguous block (no spaces) wraps to full-width lines - the
		// worst case for the content pane's height.
		m.push(conv, chatLine("12:00", "alice", strings.Repeat("x", sz.w*4), false))

		lines := strings.Split(m.View(), "\n")
		if len(lines) > sz.h {
			t.Errorf("%dx%d: view rendered %d lines, exceeds terminal height", sz.w, sz.h, len(lines))
		}
		for i, ln := range lines {
			if got := lipgloss.Width(ln); got > sz.w {
				t.Errorf("%dx%d: line %d width %d exceeds terminal width", sz.w, sz.h, i, got)
			}
		}
	}
}

// TestContentPaneMatchesSidebarHeight pins the root cause directly: the content
// column must render exactly as many lines as the sidebar so the two columns
// line up, regardless of how much text the active conversation holds.
func TestContentPaneMatchesSidebarHeight(t *testing.T) {
	const W, H = 80, 24
	m := New(&client.Client{})
	m.loggedIn = true
	m.Update(tea.WindowSizeMsg{Width: W, Height: H})
	conv := m.ensureConv(chKey("c"), "c", "#general", false)
	conv.historyLoaded = true
	m.setActive(conv.key)
	m.push(conv, chatLine("12:00", "alice", strings.Repeat("x", 600), false))

	sidebar := len(strings.Split(m.sidebarView(), "\n"))
	content := len(strings.Split(m.contentView(), "\n"))
	if content != sidebar {
		t.Errorf("content pane (%d lines) != sidebar (%d lines): columns desync", content, sidebar)
	}
	if content != m.sidebarHeight() {
		t.Errorf("content pane = %d lines, want sidebarHeight %d", content, m.sidebarHeight())
	}
}
