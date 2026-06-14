package gui

import (
	"fmt"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"

	quorumv1 "github.com/clwg/quorum/gen/quorum/v1"
)

// TestEnterChannelScrollsToBottom guards the reported regression: opening a
// channel must land on the latest message (scrolled to the bottom), not snap
// back to the top. It drives the real Fyne widget tree on a test canvas.
func TestEnterChannelScrollsToBottom(t *testing.T) {
	test.NewTempApp(t)

	a := &App{
		convs: make(map[string]*conversation),
		users: make(map[string]*quorumv1.User),
	}
	w := test.NewTempWindow(t, a.buildMain())
	w.Resize(fyne.NewSize(800, 600))

	// A channel with enough messages that the scrollback exceeds the viewport.
	conv := &conversation{key: chKey("c"), id: "c", name: "#general"}
	for i := range 120 {
		conv.msgs = append(conv.msgs, chatLine("12:00", "alice", fmt.Sprintf("message number %d", i), false))
	}
	a.convs[conv.key] = conv
	a.activeKey = conv.key

	a.rebuildMessages(conv)

	// Sanity: the harness must have laid out a viewport smaller than the content,
	// otherwise there is nothing to scroll and the assertion would be meaningless.
	viewportH := a.msgScroll.Size().Height
	contentH := a.msgBox.MinSize().Height
	if viewportH <= 0 || contentH <= viewportH {
		t.Fatalf("test setup: need content taller than viewport, got content=%v viewport=%v", contentH, viewportH)
	}

	bottom := contentH - viewportH
	if got := a.msgScroll.Offset.Y; got < bottom-1 {
		t.Fatalf("entering a channel should land at the latest message: Offset.Y = %v, want ~%v (bottom)", got, bottom)
	}
}

// TestSuppressLoadGuardsProgrammaticScroll confirms the OnScrolled handler does
// not kick off an older-history load while a programmatic scroll-to-bottom is in
// progress (which fires OnScrolled synchronously).
func TestSuppressLoadGuardsProgrammaticScroll(t *testing.T) {
	test.NewTempApp(t)

	a := &App{
		convs: make(map[string]*conversation),
		users: make(map[string]*quorumv1.User),
	}
	w := test.NewTempWindow(t, a.buildMain())
	w.Resize(fyne.NewSize(800, 600))

	conv := &conversation{key: chKey("c"), id: "c", name: "#general", historyLoaded: true, hasMore: true}
	for i := range 120 {
		conv.msgs = append(conv.msgs, chatLine("12:00", "alice", fmt.Sprintf("m%d", i), false))
	}
	a.convs[conv.key] = conv
	a.activeKey = conv.key

	a.rebuildMessages(conv) // fires OnScrolled via ScrollToBottom

	if conv.loadingOlder {
		t.Fatal("a programmatic scroll-to-bottom must not start an older-history fetch")
	}
}
