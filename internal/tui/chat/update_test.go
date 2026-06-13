package chat

import (
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	quorumv1 "github.com/layer8/quorum/gen/quorum/v1"
	"github.com/layer8/quorum/internal/client"
)

// TestIncomingDMSeedsPresenceFromDirectory covers a returning user: an
// incoming DM creates the conversation for a peer who is already online.
// That peer's presence did not change, so it emits no presence event — the
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

// TestPasswdOpensModal confirms /passwd is handled locally — it opens the
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
	m.ensureConv(chKey("a"), "a", "#alpha", false)
	m.ensureConv(chKey("b"), "b", "#beta", false)
	m.ensureConv(dmKey("c"), "c", "@carol", true)
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
