package gui

import (
	"fmt"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	quorumv1 "github.com/clwg/quorum/gen/quorum/v1"
)

// TestHighlightSegments checks the search highlighter splits a body into
// segments that preserve the original text and bold every case-insensitive
// occurrence of the query.
func TestHighlightSegments(t *testing.T) {
	text := func(segs []widget.RichTextSegment) string {
		var b strings.Builder
		for _, s := range segs {
			b.WriteString(s.(*widget.TextSegment).Text)
		}
		return b.String()
	}
	bold := func(segs []widget.RichTextSegment) int {
		n := 0
		for _, s := range segs {
			if s.(*widget.TextSegment).Style.TextStyle.Bold {
				n++
			}
		}
		return n
	}

	// Empty query: a single plain segment holding the whole body.
	segs := highlightSegments("hello world", "")
	if len(segs) != 1 || text(segs) != "hello world" || bold(segs) != 0 {
		t.Fatalf("empty query: %d segs, text %q, bold %d", len(segs), text(segs), bold(segs))
	}

	// Several case-insensitive matches: text preserved, each hit bold.
	segs = highlightSegments("Deploy then deploy", "deploy")
	if text(segs) != "Deploy then deploy" {
		t.Fatalf("text not preserved: %q", text(segs))
	}
	if bold(segs) != 2 {
		t.Fatalf("want 2 bold hits, got %d", bold(segs))
	}

	// No match: text preserved, nothing bold.
	segs = highlightSegments("lunch at noon", "deploy")
	if text(segs) != "lunch at noon" || bold(segs) != 0 {
		t.Fatalf("no match: text %q, bold %d", text(segs), bold(segs))
	}
}

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
