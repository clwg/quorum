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

// findLabel returns the first *widget.Label in obj's tree, or nil. messageRow
// nests the selectable body Label inside a Border container, so the test reaches
// it without depending on the container's child ordering.
func findLabel(obj fyne.CanvasObject) *widget.Label {
	switch o := obj.(type) {
	case *widget.Label:
		return o
	case *fyne.Container:
		for _, c := range o.Objects {
			if l := findLabel(c); l != nil {
				return l
			}
		}
	}
	return nil
}

// TestMessageRowBodySelectable checks a chat row exposes its body as a
// selectable Label carrying the message text only - not the sender or
// timestamp - so dragging to copy yields the content alone. Non-chat notices
// are selectable too, coloured by kind.
func TestMessageRowBodySelectable(t *testing.T) {
	row := messageRow(chatLine("12:00", "alice", "hello there", false))
	body := findLabel(row)
	if body == nil {
		t.Fatal("chat row has no Label for its body")
	}
	if !body.Selectable {
		t.Error("message body Label should be selectable")
	}
	if body.Text != "hello there" {
		t.Errorf("body text = %q, want the message content only (no sender/timestamp)", body.Text)
	}

	for _, tc := range []struct {
		name string
		msg  message
		want widget.Importance
	}{
		{"system", sysLine("alice joined"), widget.LowImportance},
		{"ok", okLine("session established"), widget.SuccessImportance},
		{"error", errLine("send failed"), widget.DangerImportance},
	} {
		lbl := findLabel(messageRow(tc.msg))
		if lbl == nil {
			t.Fatalf("%s row has no Label", tc.name)
		}
		if !lbl.Selectable {
			t.Errorf("%s line should be selectable", tc.name)
		}
		if lbl.Importance != tc.want {
			t.Errorf("%s line importance = %v, want %v", tc.name, lbl.Importance, tc.want)
		}
	}
}

// TestChannelMembershipHidesNonMembers checks the sidebar lists only joined
// channels while non-member channels stay reachable through the /join picker.
func TestChannelMembershipHidesNonMembers(t *testing.T) {
	a := &App{
		convs: make(map[string]*conversation),
		users: make(map[string]*quorumv1.User),
	}
	a.convs[chKey("a")] = &conversation{key: chKey("a"), id: "a", name: "#alpha", joined: true}
	a.convs[chKey("b")] = &conversation{key: chKey("b"), id: "b", name: "#beta"}
	a.rebuildOrder()

	if len(a.chOrder) != 1 || a.chOrder[0] != chKey("a") {
		t.Fatalf("sidebar channels = %v, want [%s]", a.chOrder, chKey("a"))
	}
	joinable := a.joinableChannels()
	if len(joinable) != 1 || joinable[0] != chKey("b") {
		t.Fatalf("joinable channels = %v, want [%s]", joinable, chKey("b"))
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
