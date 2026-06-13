package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/clwg/quorum/internal/client"
)

func (m *Model) updateMain(msg tea.Msg) (tea.Model, tea.Cmd) {
	// While the password modal is open it owns the keyboard and the RPC result;
	// every other message (incoming events, resize) still flows to the normal
	// handlers below so background state stays current behind the overlay.
	if m.pw != nil {
		if model, cmd, handled := m.updatePwForm(msg); handled {
			return model, cmd
		}
	}
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case EventMsg:
		return m.handleEvent(msg.Ev)
	case channelsMsg:
		if msg.err != nil {
			m.statusNote = "channel list: " + grpcErrText(msg.err)
			return m, nil
		}
		var cmds []tea.Cmd
		for _, ch := range msg.channels {
			conv := m.ensureConv(chKey(ch.GetId()), ch.GetId(), "#"+ch.GetName(), false)
			conv.joined = ch.GetIsMember()
			if conv.joined && !conv.historyLoaded {
				conv.historyLoaded = true
				cmds = append(cmds, m.fetchHistory(conv))
			}
		}
		m.rebuildOrder()
		// Finish a /join that was waiting for this list to load.
		if m.pendingJoin != "" {
			name := m.pendingJoin
			m.pendingJoin = ""
			if key := m.findChannelKey(name); key != "" {
				_, cmd := m.openConv(key)
				cmds = append(cmds, cmd)
			} else {
				m.statusNote = "no such channel #" + name
			}
		}
		return m, tea.Batch(cmds...)
	case usersMsg:
		if msg.err != nil {
			return m, nil
		}
		// Surface the whole directory in the DMs panel so any user can be
		// opened with a click or Enter, not just peers already messaged. Each
		// user becomes a DM conversation (the same one /dm would open); the
		// session still starts lazily on the first message.
		for _, u := range msg.users {
			m.users[u.GetId()] = u
			// Skip yourself and bots. Bots publish no identity key, so there is
			// no E2EE session to open with them - they can't be DM targets.
			if u.GetId() == m.client.UserID() || u.GetRole() == "bot" {
				continue
			}
			conv := m.ensureConv(dmKey(u.GetId()), u.GetId(), "@"+u.GetUsername(), true)
			conv.online = u.GetOnline()
		}
		return m, nil
	case historyMsg:
		if msg.err != nil {
			m.statusNote = "history: " + grpcErrText(msg.err)
			return m, nil
		}
		if conv, ok := m.convs[msg.convKey]; ok {
			conv.msgs = nil
			for _, cm := range msg.messages {
				conv.msgs = append(conv.msgs, chatLine(fmtTime(cm), cm.GetSenderName(), cm.GetBody(), m.isSelf(cm.GetSenderName())))
			}
			if msg.convKey == m.activeKey {
				m.refreshViewport()
			}
		}
		return m, nil
	case joinedMsg:
		conv := m.ensureConv(chKey(msg.ch.GetId()), msg.ch.GetId(), "#"+msg.ch.GetName(), false)
		conv.joined = true
		m.setActive(conv.key)
		if !conv.historyLoaded {
			conv.historyLoaded = true
			return m, m.fetchHistory(conv)
		}
		return m, nil
	case actionErrMsg:
		m.statusNote = msg.context + ": " + grpcErrText(msg.err)
		return m, nil
	}

	var cmd tea.Cmd
	if m.focus == focusInput {
		m.input, cmd = m.input.Update(msg)
	} else {
		m.vp, cmd = m.vp.Update(msg)
	}
	return m, cmd
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyTab:
		m.cycleFocus(1)
		return m, nil
	case tea.KeyShiftTab:
		m.cycleFocus(-1)
		return m, nil
	case tea.KeyPgUp, tea.KeyPgDown:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}

	if m.focus == focusChannels || m.focus == focusDMs {
		return m.handleSidebarKey(msg)
	}

	// input focused
	if msg.Type == tea.KeyEnter {
		text := strings.TrimSpace(m.input.Value())
		m.input.SetValue("")
		if text == "" {
			return m, nil
		}
		return m.submit(text)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// handleMouse routes mouse events. Over the sidebar, a left click opens the
// clicked conversation (joining a channel if needed) and the wheel scrolls
// whichever panel the cursor is over; elsewhere the wheel scrolls the message
// viewport regardless of which pane has focus.
func (m *Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.X < sidebarWidth {
		switch {
		case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress:
			if key, _ := m.sidebarHit(msg.Y); key != "" {
				return m.openConv(key)
			}
		case msg.Button == tea.MouseButtonWheelUp:
			m.scrollPanelAt(msg.Y, -1)
		case msg.Button == tea.MouseButtonWheelDown:
			m.scrollPanelAt(msg.Y, 1)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// cycleFocus advances focus through input → channels → DMs (dir 1) or the
// reverse (dir -1), keeping the message input focused only when it holds focus.
func (m *Model) cycleFocus(dir int) {
	ring := []focusArea{focusInput, focusChannels, focusDMs}
	cur := 0
	for i, f := range ring {
		if f == m.focus {
			cur = i
		}
	}
	m.focus = ring[((cur+dir)%len(ring)+len(ring))%len(ring)]
	if m.focus == focusInput {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
}

// focusedOrder returns the focused panel's key list and a pointer to its
// selection cursor. It defaults to the channels panel.
func (m *Model) focusedOrder() ([]string, *int) {
	if m.focus == focusDMs {
		return m.dmOrder, &m.dmIdx
	}
	return m.chOrder, &m.chIdx
}

// handleSidebarKey drives selection within the focused panel: up/down (or k/j)
// move the cursor, home/end jump to the ends, left/right (or h/l) switch panels,
// and Enter opens the selected conversation.
func (m *Model) handleSidebarKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	order, idx := m.focusedOrder()
	switch msg.String() {
	case "up", "k":
		if *idx > 0 {
			*idx--
			m.ensureVisible()
		}
	case "down", "j":
		if *idx < len(order)-1 {
			*idx++
			m.ensureVisible()
		}
	case "home", "g":
		*idx = 0
		m.ensureVisible()
	case "end", "G":
		if len(order) > 0 {
			*idx = len(order) - 1
			m.ensureVisible()
		}
	case "left", "h", "right", "l":
		if m.focus == focusChannels {
			m.focus = focusDMs
		} else {
			m.focus = focusChannels
		}
	case "enter":
		if *idx < len(order) {
			return m.openConv(order[*idx])
		}
	}
	return m, nil
}

// scrollPanelAt scrolls the panel under the given Y by delta rows, independent
// of the selection cursor.
func (m *Model) scrollPanelAt(y, delta int) {
	chListH, dmListH := m.listHeights()
	if y <= chListH { // CHANNELS header + channel window
		m.chScroll = clampScroll(m.chScroll+delta, chListH, len(m.chOrder))
	} else {
		m.dmScroll = clampScroll(m.dmScroll+delta, dmListH, len(m.dmOrder))
	}
}

// findChannelKey returns the conversation key for a channel matching the
// given name (with or without a leading '#'), compared case-insensitively.
// Returns "" if no channel matches.
func (m *Model) findChannelKey(name string) string {
	name = strings.TrimPrefix(name, "#")
	for key, conv := range m.convs {
		if !conv.isDM && strings.EqualFold(strings.TrimPrefix(conv.name, "#"), name) {
			return key
		}
	}
	return ""
}

// openConv activates a sidebar entry, joining channels as needed.
func (m *Model) openConv(key string) (tea.Model, tea.Cmd) {
	conv := m.convs[key]
	if conv == nil {
		return m, nil
	}
	if !conv.isDM && !conv.joined {
		channelID := conv.id
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ch, err := m.client.JoinChannel(ctx, channelID)
			if err != nil {
				return actionErrMsg{context: "join", err: err}
			}
			return joinedMsg{ch: ch}
		}
	}
	m.setActive(key)
	if !conv.isDM && !conv.historyLoaded {
		conv.historyLoaded = true
		return m, m.fetchHistory(conv)
	}
	return m, nil
}

func (m *Model) setActive(key string) {
	m.activeKey = key
	if conv := m.convs[key]; conv != nil {
		conv.unread = 0
	}
	// Move the owning panel's cursor onto the opened conversation so it is
	// highlighted when focus returns to the sidebar.
	for i, k := range m.chOrder {
		if k == key {
			m.chIdx = i
		}
	}
	for i, k := range m.dmOrder {
		if k == key {
			m.dmIdx = i
		}
	}
	m.focus = focusInput
	m.input.Focus()
	m.ensureVisible()
	m.refreshViewport()
}

// submit handles the input line: local slash commands or a message send.
func (m *Model) submit(text string) (tea.Model, tea.Cmd) {
	if strings.HasPrefix(text, "/") {
		fields := strings.Fields(text)
		switch fields[0] {
		case "/help":
			m.showHelp()
			return m, nil
		case "/quit":
			return m, tea.Quit
		case "/create":
			if len(fields) < 2 {
				m.statusNote = "usage: /create <name>"
				return m, nil
			}
			name := fields[1]
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				ch, err := m.client.CreateChannel(ctx, name)
				if err != nil {
					return actionErrMsg{context: "create", err: err}
				}
				return joinedMsg{ch: ch}
			}
		case "/join":
			if len(fields) < 2 {
				m.statusNote = "usage: /join <name>"
				return m, nil
			}
			// Accept "#general" or "general" (the '#' is optional), matched
			// case-insensitively.
			target := strings.TrimPrefix(fields[1], "#")
			if key := m.findChannelKey(target); key != "" {
				return m.openConv(key)
			}
			// The local list may be stale (e.g. someone just created it):
			// refresh and finish the join once the new list arrives.
			m.pendingJoin = target
			m.statusNote = "looking for #" + target + "…"
			return m, m.fetchChannels()
		case "/leave":
			conv := m.active()
			if conv == nil || conv.isDM {
				m.statusNote = "/leave works in a channel"
				return m, nil
			}
			channelID, key := conv.id, conv.key
			delete(m.convs, key)
			m.rebuildOrder()
			m.activeKey = ""
			m.refreshViewport()
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := m.client.LeaveChannel(ctx, channelID); err != nil {
					return actionErrMsg{context: "leave", err: err}
				}
				return nil
			}
		case "/dm":
			if len(fields) < 2 {
				m.statusNote = "usage: /dm <username>"
				return m, nil
			}
			return m.openDM(fields[1])
		case "/passwd":
			// Open the modal form rather than reading a password from the
			// command line: nothing is sent until the user fills it in and
			// confirms, so a stray "/passwd" can never change anything or leak
			// into a channel.
			return m.openPwForm()
		}
		// Unknown slash commands (a bot's own command, or the built-in
		// /commands bot-command listing) fall through to the channel so
		// bots and the server can see them.
	}

	conv := m.active()
	if conv == nil {
		m.statusNote = "select a conversation first (click one, or Tab then arrows + Enter)"
		return m, nil
	}
	if conv.isDM {
		peerID, peerName := conv.id, strings.TrimPrefix(conv.name, "@")
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := m.client.SendDM(ctx, peerID, peerName, text); err != nil {
				return actionErrMsg{context: "dm", err: err}
			}
			return nil
		}
	}
	channelID := conv.id
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.client.SendChannelMessage(ctx, channelID, text); err != nil {
			return actionErrMsg{context: "send", err: err}
		}
		return nil
	}
}

func (m *Model) openDM(username string) (tea.Model, tea.Cmd) {
	username = strings.TrimPrefix(username, "@")
	for _, u := range m.users {
		if strings.EqualFold(u.GetUsername(), username) {
			if u.GetId() == m.client.UserID() {
				m.statusNote = "that's you"
				return m, nil
			}
			conv := m.ensureConv(dmKey(u.GetId()), u.GetId(), "@"+u.GetUsername(), true)
			m.setActive(conv.key)
			return m, nil
		}
	}
	m.statusNote = "unknown user " + username
	return m, m.fetchUsers()
}

// --- password change modal ---

// pwFields labels the three modal inputs, in order.
var pwFields = []string{"current password", "new password", "confirm new password"}

// openPwForm builds and shows the /passwd modal with three masked fields.
func (m *Model) openPwForm() (tea.Model, tea.Cmd) {
	inputs := make([]textinput.Model, len(pwFields))
	for i, ph := range pwFields {
		in := textinput.New()
		in.Placeholder = ph
		in.EchoMode = textinput.EchoPassword
		in.CharLimit = 256
		in.Prompt = ""
		inputs[i] = in
	}
	inputs[0].Focus()
	m.pw = &pwForm{inputs: inputs}
	m.input.Blur()
	return m, textinput.Blink
}

// closePwForm dismisses the modal and returns focus to the message input.
func (m *Model) closePwForm() {
	m.pw = nil
	m.focus = focusInput
	m.input.Focus()
}

// pwFocus moves the modal's focus by delta, wrapping, and updates which field
// shows the blinking cursor.
func (m *Model) pwFocus(delta int) {
	n := len(m.pw.inputs)
	m.pw.focus = ((m.pw.focus+delta)%n + n) % n
	for i := range m.pw.inputs {
		if i == m.pw.focus {
			m.pw.inputs[i].Focus()
		} else {
			m.pw.inputs[i].Blur()
		}
	}
}

// updatePwForm drives the modal. It returns handled=true for the keystrokes and
// the RPC result it consumes, and handled=false for everything else so those
// messages fall through to the normal update path.
func (m *Model) updatePwForm(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case pwResultMsg:
		m.pw.busy = false
		if msg.err != nil {
			m.pw.err = grpcErrText(msg.err)
			return m, nil, true
		}
		m.closePwForm()
		m.statusNote = "password changed"
		return m, nil, true
	case tea.KeyMsg:
		if m.pw.busy {
			return m, nil, true // ignore input while the RPC is in flight
		}
		switch msg.Type {
		case tea.KeyEsc:
			m.closePwForm()
			return m, nil, true
		case tea.KeyTab, tea.KeyDown:
			m.pwFocus(1)
			return m, nil, true
		case tea.KeyShiftTab, tea.KeyUp:
			m.pwFocus(-1)
			return m, nil, true
		case tea.KeyEnter:
			return m.submitPwForm()
		}
		var cmd tea.Cmd
		m.pw.inputs[m.pw.focus], cmd = m.pw.inputs[m.pw.focus].Update(msg)
		return m, cmd, true
	}
	return m, nil, false
}

// submitPwForm validates the three fields locally, then fires the RPC. Local
// checks keep an obvious mistake from costing a round trip; the server is still
// the authority (it re-checks length and the current password).
func (m *Model) submitPwForm() (tea.Model, tea.Cmd, bool) {
	old := m.pw.inputs[0].Value()
	next := m.pw.inputs[1].Value()
	confirm := m.pw.inputs[2].Value()
	switch {
	case old == "":
		m.pw.err = "enter your current password"
	case len(next) < 8:
		m.pw.err = "new password must be at least 8 characters"
	case next != confirm:
		m.pw.err = "new passwords do not match"
	case next == old:
		m.pw.err = "new password must differ from the current one"
	default:
		m.pw.err = ""
		m.pw.busy = true
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return pwResultMsg{err: m.client.ChangePassword(ctx, old, next)}
		}, true
	}
	return m, nil, true
}

// helpLines is the built-in /help output: the client's own slash commands and
// key bindings. Commands offered by bots are listed separately by /commands.
var helpLines = []string{
	"quorum commands:",
	"  /create <name>   create a channel and open it",
	"  /join <name>     join an existing channel",
	"  /leave           leave the current channel",
	"  /dm <user>       open an end-to-end-encrypted direct message",
	"  /passwd          change your password (opens a private form)",
	"  /commands        list commands offered by bots",
	"  /help            show this help",
	"  /quit            exit quorum",
	"keys:",
	"  Tab/Shift+Tab    cycle focus: input → channels → DMs",
	"  up/down or j/k   move within the focused panel",
	"  left/right h/l   switch between the channels and DMs panels",
	"  Enter            open the selected conversation (joins if needed)",
	"  click            open a channel or DM in the sidebar",
	"  wheel over panel scroll the channels or DMs list",
	"  PgUp/PgDn        scroll the message history",
	"  Ctrl+C           quit",
}

// showHelp renders the built-in command help. It lands in the active
// conversation's scrollback when one is open, and directly in the viewport
// otherwise (e.g. right after login, before a conversation is selected).
func (m *Model) showHelp() {
	if conv := m.active(); conv != nil {
		for _, line := range helpLines {
			m.push(conv, helpLine(line))
		}
		return
	}
	rendered := make([]string, len(helpLines))
	for i, line := range helpLines {
		rendered[i] = renderMessage(helpLine(line), m.contentWidth())
	}
	m.vp.SetContent(strings.Join(rendered, "\n"))
	m.vp.GotoBottom()
}

// handleEvent processes events from the client pump.
func (m *Model) handleEvent(ev client.Event) (tea.Model, tea.Cmd) {
	switch ev := ev.(type) {
	case client.ConnStateEvent:
		m.connState = ev.State
		if ev.Err != nil {
			m.statusNote = grpcErrText(ev.Err)
		}
		return m, nil
	case client.ResyncEvent:
		// Mark histories stale so they reload.
		for _, conv := range m.convs {
			if !conv.isDM {
				conv.historyLoaded = false
			}
		}
		return m, tea.Batch(m.fetchChannels(), m.fetchUsers())
	case client.ChannelMessageEvent:
		cm := ev.Msg
		conv := m.ensureConv(chKey(cm.GetChannelId()), cm.GetChannelId(), "#"+cm.GetChannelId(), false)
		m.push(conv, chatLine(fmtTime(cm), cm.GetSenderName(), cm.GetBody(), m.isSelf(cm.GetSenderName())))
		return m, nil
	case client.DirectMessageEvent:
		conv := m.ensureConv(dmKey(ev.PeerID), ev.PeerID, "@"+ev.PeerName, true)
		who := ev.PeerName
		if ev.Outgoing {
			who = m.client.Username()
		}
		m.push(conv, chatLine(time.Now().Format("15:04"), who, ev.Text, ev.Outgoing))
		return m, nil
	case client.DMSessionEvent:
		conv := m.ensureConv(dmKey(ev.PeerID), ev.PeerID, "@"+ev.PeerName, true)
		conv.established = ev.Established
		if ev.Fingerprint != "" {
			conv.fingerprint = ev.Fingerprint
		}
		if ev.Err != nil {
			m.push(conv, errLine(ev.Err.Error()))
		} else if ev.Established {
			m.push(conv, okLine(fmt.Sprintf("🔒 encrypted session established - their key: %s", conv.fingerprint)))
		} else {
			m.push(conv, sysLine("🔓 session closed"))
		}
		return m, nil
	case client.PresenceEvent:
		p := ev.Presence
		u, known := m.users[p.GetUserId()]
		if !known {
			// A user we haven't seen before just came online. Presence carries
			// no role, so pull the directory to add them to the roster with the
			// bot filter applied (see usersMsg). A going-offline event for an
			// unknown user needs no action.
			if p.GetOnline() {
				return m, m.fetchUsers()
			}
			return m, nil
		}
		u.Online = p.GetOnline()
		if conv, ok := m.convs[dmKey(p.GetUserId())]; ok {
			conv.online = p.GetOnline()
		}
		return m, nil
	case client.ChannelEventEvent:
		ce := ev.Event
		ch := ce.GetChannel()
		conv := m.ensureConv(chKey(ch.GetId()), ch.GetId(), "#"+ch.GetName(), false)
		switch ce.GetType() {
		case 1: // created
		case 2:
			m.push(conv, sysLine(fmt.Sprintf("→ %s joined", ce.GetUsername())))
		case 3:
			m.push(conv, sysLine(fmt.Sprintf("← %s left", ce.GetUsername())))
		}
		return m, nil
	case client.SystemEvent:
		if t := ev.Notice.GetText(); t != "" && t != "connected" {
			if conv := m.active(); conv != nil {
				m.push(conv, sysLine(t))
			} else {
				m.statusNote = t
			}
		}
		return m, nil
	case client.ErrorEvent:
		m.statusNote = ev.Context + ": " + ev.Err.Error()
		return m, nil
	}
	return m, nil
}
