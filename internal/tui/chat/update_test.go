package chat

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	quorumv1 "github.com/clwg/quorum/gen/quorum/v1"
	"github.com/clwg/quorum/internal/client"
)

// makeHistory builds n channel messages with ascending ids starting at startID,
// matching the server's ascending-by-id history ordering.
func makeHistory(startID int64, n int) []*quorumv1.ChannelMessage {
	msgs := make([]*quorumv1.ChannelMessage, n)
	for i := range n {
		id := startID + int64(i)
		msgs[i] = &quorumv1.ChannelMessage{Id: id, SenderName: "alice", Body: fmt.Sprintf("m%d", id)}
	}
	return msgs
}

// TestChannelHistoryPagination covers scroll-up loading of older messages: an
// initial full page leaves more history available; reaching the top starts
// exactly one fetch; a short older page prepends in order and exhausts history.
func TestChannelHistoryPagination(t *testing.T) {
	m := New(nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	conv := m.ensureConv(chKey("c"), "c", "#general", false)
	conv.historyLoaded = true
	m.setActive(conv.key)

	// A full initial page records the cursor and that more history may exist.
	m.applyInitialHistory(conv, makeHistory(101, historyPageSize))
	if got := len(conv.msgs); got != historyPageSize {
		t.Fatalf("initial msgs = %d, want %d", got, historyPageSize)
	}
	if conv.oldestID != 101 {
		t.Fatalf("oldestID = %d, want 101", conv.oldestID)
	}
	if !conv.hasMore {
		t.Fatal("a full first page should leave hasMore=true")
	}

	// Scrolled away from the top: no fetch, even with more history.
	m.vp.SetYOffset(historyScrollThreshold + 50)
	if cmd := m.maybeLoadOlder(); cmd != nil || conv.loadingOlder {
		t.Fatal("not near the top: no older-history fetch expected")
	}

	// At the top with more history: a fetch starts, exactly once.
	m.vp.SetYOffset(0)
	if cmd := m.maybeLoadOlder(); cmd == nil {
		t.Fatal("at top with more history: expected an older-history fetch")
	}
	if !conv.loadingOlder {
		t.Fatal("maybeLoadOlder should mark the fetch in flight")
	}
	if cmd := m.maybeLoadOlder(); cmd != nil {
		t.Fatal("a fetch is already in flight: no second fetch expected")
	}

	// The history handler clears the in-flight flag before applying the page.
	conv.loadingOlder = false
	m.applyOlderHistory(conv, makeHistory(1, 10))
	if got := len(conv.msgs); got != historyPageSize+10 {
		t.Fatalf("after prepend msgs = %d, want %d", got, historyPageSize+10)
	}
	if conv.msgs[0].body != "m1" {
		t.Fatalf("oldest rendered message = %q, want m1", conv.msgs[0].body)
	}
	if conv.oldestID != 1 {
		t.Fatalf("oldestID = %d, want 1", conv.oldestID)
	}
	if conv.hasMore {
		t.Fatal("a short page (< full) should set hasMore=false")
	}

	// History exhausted: reaching the top no longer fetches.
	m.vp.SetYOffset(0)
	if cmd := m.maybeLoadOlder(); cmd != nil {
		t.Fatal("history exhausted: no fetch expected")
	}
}

// TestMaybeLoadOlderSkipsDMs confirms DMs (which have no server-side history)
// never trigger an older-history fetch even when scrolled to the top.
func TestMaybeLoadOlderSkipsDMs(t *testing.T) {
	m := New(nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	conv := m.ensureConv(dmKey("p"), "p", "@peer", true)
	conv.historyLoaded = true
	conv.hasMore = true
	m.setActive(conv.key)
	m.vp.SetYOffset(0)
	if cmd := m.maybeLoadOlder(); cmd != nil || conv.loadingOlder {
		t.Fatal("DMs should never fetch channel history")
	}
}

// TestIncomingDMSeedsPresenceFromDirectory covers a returning user: an
// incoming DM creates the conversation for a peer who is already online.
// That peer's presence did not change, so it emits no presence event - the
// online indicator can only come from the directory snapshot in m.users.
func TestIncomingDMSeedsPresenceFromDirectory(t *testing.T) {
	m := New(nil)
	const onID, offID = "online-id", "offline-id"
	m.users[onID] = &quorumv1.User{Id: onID, Username: "online", Online: true}
	m.users[offID] = &quorumv1.User{Id: offID, Username: "offline", Online: false}

	m.handleEvent(client.DirectMessageEvent{PeerID: onID, PeerName: "online", Text: "hi"})
	m.handleEvent(client.DirectMessageEvent{PeerID: offID, PeerName: "offline", Text: "hi"})

	if conv := m.convs[dmKey(onID)]; conv == nil || !conv.online {
		t.Fatalf("online peer: want online indicator, got %+v", conv)
	}
	if conv := m.convs[dmKey(offID)]; conv == nil || conv.online {
		t.Fatalf("offline peer: want offline indicator, got %+v", conv)
	}

	// A later presence event still flips the indicator.
	m.handleEvent(client.PresenceEvent{Presence: &quorumv1.PresenceEvent{UserId: offID, Username: "offline", Online: true}})
	if conv := m.convs[dmKey(offID)]; conv == nil || !conv.online {
		t.Fatalf("presence online: want online indicator, got %+v", conv)
	}
}

// TestUnknownUserPresenceRefreshesRoster covers the live roster: a presence
// event for a user already in the directory just updates their state, while one
// for a previously unseen user coming online triggers a directory refresh (which
// adds them with the bot filter applied). A going-offline event for an unknown
// user does nothing.
func TestUnknownUserPresenceRefreshesRoster(t *testing.T) {
	m := New(nil)
	m.users["known"] = &quorumv1.User{Id: "known", Username: "known"}

	if _, cmd := m.handleEvent(client.PresenceEvent{Presence: &quorumv1.PresenceEvent{UserId: "known", Online: true}}); cmd != nil {
		t.Fatalf("known user presence should issue no command")
	}
	if _, cmd := m.handleEvent(client.PresenceEvent{Presence: &quorumv1.PresenceEvent{UserId: "newbie", Username: "newbie", Online: true}}); cmd == nil {
		t.Fatalf("unknown user coming online should refresh the roster")
	}
	if _, cmd := m.handleEvent(client.PresenceEvent{Presence: &quorumv1.PresenceEvent{UserId: "ghost", Online: false}}); cmd != nil {
		t.Fatalf("unknown user going offline should issue no command")
	}
}

// TestSearchCommandValidation covers the local guards before any RPC: /search
// only works in a channel and needs a non-empty query.
func TestSearchCommandValidation(t *testing.T) {
	m := New(nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// No active conversation: rejected with a note, no overlay.
	m.submit("/search hello")
	if m.search != nil {
		t.Fatal("/search with no channel should not open the overlay")
	}
	if m.statusNote == "" {
		t.Fatal("/search with no channel should set a status note")
	}

	// In a DM: same rejection.
	dm := m.ensureConv(dmKey("p"), "p", "@peer", true)
	m.setActive(dm.key)
	m.submit("/search hello")
	if m.search != nil {
		t.Fatal("/search in a DM should not open the overlay")
	}

	// In a channel, a blank query is rejected before any search command runs.
	conv := m.ensureConv(chKey("c"), "c", "#general", false)
	m.setActive(conv.key)
	if _, cmd := m.submit("/search    "); cmd != nil {
		t.Fatal("blank /search query should not start a search")
	}
	if m.statusNote != "usage: /search <query>" {
		t.Fatalf("blank query status = %q", m.statusNote)
	}
}

// TestSearchOverlay covers the results overlay: openSearch shows it with the
// match count, Esc closes it, and unrelated messages are not consumed so
// background state keeps updating behind it.
func TestSearchOverlay(t *testing.T) {
	m := New(nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	m.openSearch("m2", makeHistory(1, 3))
	if m.search == nil {
		t.Fatal("openSearch should open the overlay")
	}
	if m.search.count != 3 {
		t.Fatalf("match count = %d, want 3", m.search.count)
	}

	if _, _, handled := m.updateSearch(joinedMsg{}); handled {
		t.Fatal("overlay should not consume unrelated messages")
	}
	if _, _, handled := m.updateSearch(tea.KeyMsg{Type: tea.KeyEsc}); !handled {
		t.Fatal("esc should be handled by the overlay")
	}
	if m.search != nil {
		t.Fatal("esc should close the overlay")
	}
}

// TestChannelMembershipHidesNonMembers checks that processing a channel list
// keeps only joined channels in the sidebar while non-member channels stay
// reachable through the /join picker.
func TestChannelMembershipHidesNonMembers(t *testing.T) {
	m := New(nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	m.updateMain(channelsMsg{channels: []*quorumv1.Channel{
		{Id: "a", Name: "alpha", IsMember: true},
		{Id: "b", Name: "beta", IsMember: false},
	}})

	if len(m.chOrder) != 1 || m.chOrder[0] != chKey("a") {
		t.Fatalf("sidebar channels = %v, want [%s]", m.chOrder, chKey("a"))
	}
	joinable := m.joinableChannels()
	if len(joinable) != 1 || joinable[0] != chKey("b") {
		t.Fatalf("joinable channels = %v, want [%s]", joinable, chKey("b"))
	}
}

// TestJoinPickerOpensAndJoins covers the /join picker: a bare /join opens it once
// the channel list refreshes, and Enter on a selection joins that channel.
func TestJoinPickerOpensAndJoins(t *testing.T) {
	m := New(nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// A bare /join arms the picker and asks for a fresh list.
	m.submit("/join")
	if !m.pendingJoinPicker {
		t.Fatal("/join should arm the picker pending a refresh")
	}

	// The refreshed list arrives with one joinable channel; the picker opens.
	m.updateMain(channelsMsg{channels: []*quorumv1.Channel{
		{Id: "b", Name: "beta", IsMember: false},
	}})
	if m.pendingJoinPicker {
		t.Fatal("the pending flag should clear once the list arrives")
	}
	if m.joinP == nil || len(m.joinP.keys) != 1 || m.joinP.keys[0] != chKey("b") {
		t.Fatalf("picker = %+v, want one entry for %s", m.joinP, chKey("b"))
	}

	// Enter joins the selected channel: the picker closes and openConv issues a
	// join command (a command is returned for an unjoined channel).
	_, cmd, handled := m.updateJoinPicker(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled {
		t.Fatal("enter should be handled by the picker")
	}
	if m.joinP != nil {
		t.Fatal("enter should close the picker")
	}
	if cmd == nil {
		t.Fatal("enter on an unjoined channel should issue a join command")
	}
}

// TestJoinedChannelAppearsInSidebar checks that joining (or creating) a channel
// surfaces it in the sidebar right away, since membership flips after the
// conversation already exists.
func TestJoinedChannelAppearsInSidebar(t *testing.T) {
	m := New(nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// A non-member channel is hidden from the sidebar.
	m.updateMain(channelsMsg{channels: []*quorumv1.Channel{
		{Id: "b", Name: "beta", IsMember: false},
	}})
	if len(m.chOrder) != 0 {
		t.Fatalf("a non-member channel should be hidden, chOrder = %v", m.chOrder)
	}

	// Joining it surfaces it in the sidebar immediately.
	m.updateMain(joinedMsg{ch: &quorumv1.Channel{Id: "b", Name: "beta"}})
	if len(m.chOrder) != 1 || m.chOrder[0] != chKey("b") {
		t.Fatalf("after join chOrder = %v, want [%s]", m.chOrder, chKey("b"))
	}
}

// TestJoinPickerEmpty checks that opening the picker with no joinable channels
// leaves a status note instead of an empty overlay.
func TestJoinPickerEmpty(t *testing.T) {
	m := New(nil)
	m.openJoinPicker()
	if m.joinP != nil {
		t.Fatal("the picker should not open when there is nothing to join")
	}
	if m.statusNote == "" {
		t.Fatal("an empty picker should leave a status note")
	}
}

// TestHighlightLike checks the match highlighter preserves the body's text
// exactly (correct byte slicing) across zero, one, and several case-insensitive
// matches. Styling is stripped without a terminal, so equality is on content.
func TestHighlightLike(t *testing.T) {
	cases := []struct{ body, query string }{
		{"hello world", ""},                  // no query: unchanged
		{"lunch at noon", "deploy"},          // no match: unchanged
		{"Deploy a deploy DEPLOY", "deploy"}, // several case-insensitive matches
		{"deploy", "deploy"},                 // whole body is the match
	}
	for _, tc := range cases {
		if got := highlightLike(tc.body, tc.query); got != tc.body {
			t.Errorf("highlightLike(%q, %q) text = %q, want %q", tc.body, tc.query, got, tc.body)
		}
	}
	// Ensure we are exercising the highlighting path, not just the empty-query
	// shortcut: a real match still round-trips to the same text.
	if !strings.Contains(highlightLike("the deploy", "deploy"), "deploy") {
		t.Fatal("highlight should retain the matched text")
	}
}

// TestPasswdOpensModal confirms /passwd is handled locally - it opens the
// three-field modal instead of falling through to be sent into the active
// channel, which would leak a typed password.
func TestPasswdOpensModal(t *testing.T) {
	m := New(nil)
	m.ensureConv(chKey("c"), "c", "#general", false)
	m.setActive(chKey("c"))

	m.submit("/passwd")
	if m.pw == nil {
		t.Fatal("/passwd should open the password modal")
	}
	if len(m.pw.inputs) != 3 {
		t.Fatalf("modal wants 3 fields, got %d", len(m.pw.inputs))
	}
	for i, in := range m.pw.inputs {
		if in.EchoMode != textinput.EchoPassword {
			t.Fatalf("field %d should be masked", i)
		}
	}
}

// TestPasswdFormValidation walks the local pre-flight checks. Each invalid case
// must keep the modal open with an error and must not fire the RPC.
func TestPasswdFormValidation(t *testing.T) {
	m := New(nil)
	m.openPwForm()
	set := func(cur, next, confirm string) {
		m.pw.inputs[0].SetValue(cur)
		m.pw.inputs[1].SetValue(next)
		m.pw.inputs[2].SetValue(confirm)
		m.pw.err = ""
	}
	cases := []struct {
		name             string
		cur, next, confm string
	}{
		{"empty current", "", "newpassword1", "newpassword1"},
		{"short new", "password123", "short", "short"},
		{"mismatch", "password123", "newpassword1", "different123"},
		{"same as current", "password123", "password123", "password123"},
	}
	for _, tc := range cases {
		set(tc.cur, tc.next, tc.confm)
		m.submitPwForm()
		if m.pw == nil {
			t.Fatalf("%s: modal should stay open", tc.name)
		}
		if m.pw.err == "" {
			t.Fatalf("%s: want a validation error", tc.name)
		}
		if m.pw.busy {
			t.Fatalf("%s: must not start the RPC", tc.name)
		}
	}
}

// TestPasswdFormEscCloses checks Esc dismisses the modal, while an unrelated
// message is not consumed so background state keeps updating behind it.
func TestPasswdFormEscCloses(t *testing.T) {
	m := New(nil)
	m.openPwForm()

	if _, _, handled := m.updatePwForm(joinedMsg{}); handled {
		t.Fatal("modal should not consume unrelated messages")
	}
	if _, _, handled := m.updatePwForm(tea.KeyMsg{Type: tea.KeyEsc}); !handled {
		t.Fatal("esc should be handled by the modal")
	}
	if m.pw != nil {
		t.Fatal("esc should close the modal")
	}
}

// sidebarFixture builds a sidebar with two channels and one DM. With the
// default (zero) window size the channels and DMs panels each get two rows, so
// sidebarView draws:
//
//	row 0  CHANNELS
//	row 1  #alpha
//	row 2  #beta
//	row 3  (blank separator)
//	row 4  DMS
//	row 5  @carol
//	row 6  (blank padding)
func sidebarFixture() *Model {
	m := New(nil)
	// Channels appear in the sidebar only once joined.
	m.ensureConv(chKey("a"), "a", "#alpha", false).joined = true
	m.ensureConv(chKey("b"), "b", "#beta", false).joined = true
	m.ensureConv(dmKey("c"), "c", "@carol", true)
	m.rebuildOrder()
	return m
}

// TestSidebarHitMapsRows checks that a click's Y coordinate resolves to the
// right conversation and panel, and that headers, separators, and padding rows
// resolve to nothing.
func TestSidebarHitMapsRows(t *testing.T) {
	m := sidebarFixture()
	type hit struct {
		key   string
		panel focusArea
	}
	want := map[int]hit{
		0:  {"", focusInput},            // CHANNELS header
		1:  {chKey("a"), focusChannels}, // #alpha
		2:  {chKey("b"), focusChannels}, // #beta
		3:  {"", focusInput},            // blank separator
		4:  {"", focusInput},            // DMS header
		5:  {dmKey("c"), focusDMs},      // @carol
		6:  {"", focusInput},            // padding past the last DM
		99: {"", focusInput},            // far out of range
	}
	for y, exp := range want {
		key, panel := m.sidebarHit(y)
		if key != exp.key || (key != "" && panel != exp.panel) {
			t.Errorf("sidebarHit(%d) = (%q, %d), want (%q, %d)", y, key, panel, exp.key, exp.panel)
		}
	}
}

// TestMouseClickOpensConversation covers the core navigation path: a left
// click on a DM row activates that conversation, regardless of prior focus.
func TestMouseClickOpensConversation(t *testing.T) {
	m := sidebarFixture()
	m.focus = focusInput

	click := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 2, Y: 5}
	if _, cmd := m.handleMouse(click); cmd != nil {
		t.Fatalf("opening an existing DM should issue no command, got %T", cmd())
	}
	if m.activeKey != dmKey("c") {
		t.Fatalf("activeKey = %q, want %q", m.activeKey, dmKey("c"))
	}

	// A click on a header row leaves the active conversation untouched.
	header := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 2, Y: 0}
	m.handleMouse(header)
	if m.activeKey != dmKey("c") {
		t.Fatalf("after header click activeKey = %q, want %q", m.activeKey, dmKey("c"))
	}
}

// selectionFixture builds a logged-in model with one message whose body
// "HELLOWORLD" sits at content columns 17..26 - on an 80x24 screen that is
// screen columns 45..54 of the first viewport row (y=1).
func selectionFixture(t *testing.T) *Model {
	t.Helper()
	m := New(&client.Client{})
	m.loggedIn = true
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	conv := m.ensureConv(chKey("c"), "c", "#general", false)
	conv.historyLoaded = true
	m.setActive(conv.key)
	m.push(conv, chatLine("12:00", "alice", "HELLOWORLD", false))
	return m
}

func leftMouse(action tea.MouseAction, x, y int) tea.MouseMsg {
	return tea.MouseMsg{Button: tea.MouseButtonLeft, Action: action, X: x, Y: y}
}

// TestCellAtMapsScreenToContent pins the layout offsets: the cell where the
// message body is drawn on screen must map back to its content row/column. If
// the sidebar width, borders, or padding change, this fails loudly.
func TestCellAtMapsScreenToContent(t *testing.T) {
	m := selectionFixture(t)
	wantRow, wantCol := -1, -1
	for y, ln := range strings.Split(m.View(), "\n") {
		if idx := strings.Index(ansi.Strip(ln), "HELLOWORLD"); idx >= 0 {
			wantRow, wantCol = y, idx
			break
		}
	}
	if wantRow < 0 {
		t.Fatal("did not find the message on screen")
	}
	row, col, ok := m.cellAt(wantCol, wantRow)
	if !ok {
		t.Fatalf("cellAt(%d,%d) should map a viewport cell", wantCol, wantRow)
	}
	if row != 0 || col != 17 {
		t.Fatalf("cellAt(%d,%d) = (row %d, col %d), want content (0, 17)", wantCol, wantRow, row, col)
	}
}

// TestMouseSelectionCopiesSpan covers a left-button drag over the message pane:
// it selects the dragged span and returns a clipboard command on release.
func TestMouseSelectionCopiesSpan(t *testing.T) {
	m := selectionFixture(t)

	m.handleMouse(leftMouse(tea.MouseActionPress, 45, 1))
	if !m.selDragging {
		t.Fatal("a left press in the message pane should start a selection")
	}
	m.handleMouse(leftMouse(tea.MouseActionMotion, 55, 1))
	_, cmd := m.handleMouse(leftMouse(tea.MouseActionRelease, 55, 1))
	if cmd == nil {
		t.Fatal("releasing a non-empty selection should return a copy command")
	}
	if m.selDragging {
		t.Fatal("release should end the drag")
	}
	if got := m.selectedText(); got != "HELLOWORLD" {
		t.Fatalf("selected text = %q, want %q", got, "HELLOWORLD")
	}
}

// TestMouseSelectionReleaseOverSidebar guards a drag that ends outside the
// message pane: releasing over the sidebar must still finish the selection
// rather than stranding the drag.
func TestMouseSelectionReleaseOverSidebar(t *testing.T) {
	m := selectionFixture(t)

	m.handleMouse(leftMouse(tea.MouseActionPress, 45, 1))
	m.handleMouse(leftMouse(tea.MouseActionMotion, 55, 1))
	// Release with x in the sidebar region (x < sidebarWidth).
	_, cmd := m.handleMouse(leftMouse(tea.MouseActionRelease, 2, 1))
	if m.selDragging {
		t.Fatal("releasing over the sidebar should still end the drag")
	}
	if cmd == nil {
		t.Fatal("the selection should still be copied on release")
	}
}

// TestMouseClickClearsSelection checks that a plain click (press then release
// with no drag) copies nothing and dismisses any existing selection.
func TestMouseClickClearsSelection(t *testing.T) {
	m := selectionFixture(t)

	// Make a selection first.
	m.handleMouse(leftMouse(tea.MouseActionPress, 45, 1))
	m.handleMouse(leftMouse(tea.MouseActionMotion, 55, 1))
	m.handleMouse(leftMouse(tea.MouseActionRelease, 55, 1))
	if m.sel == nil {
		t.Fatal("expected a selection after a drag")
	}

	// A click clears it without copying.
	m.handleMouse(leftMouse(tea.MouseActionPress, 45, 1))
	if _, cmd := m.handleMouse(leftMouse(tea.MouseActionRelease, 45, 1)); cmd != nil {
		t.Fatal("a click should not copy")
	}
	if m.sel != nil {
		t.Fatal("a click should clear the selection")
	}
}
