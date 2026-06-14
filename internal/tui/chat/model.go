// Package chat implements the user-facing TUI: a login form, a sidebar of
// channels and DMs, a message viewport, and an input line. All gRPC
// traffic enters the Update loop as tea.Msgs delivered by the client
// pump; outbound actions run as tea.Cmds.
package chat

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	quorumv1 "github.com/clwg/quorum/gen/quorum/v1"
	"github.com/clwg/quorum/internal/client"
)

const sidebarWidth = 24

// historyPageSize is how many channel messages each history fetch requests -
// both the initial load when a channel is opened and each older page loaded by
// scrolling the scrollback up.
const historyPageSize = 100

// historyScrollThreshold is how close (in viewport lines) the scrollback must be
// to the top before scrolling up fetches the next older page.
const historyScrollThreshold = 3

// EventMsg wraps a client event for the Update loop.
type EventMsg struct{ Ev client.Event }

type loginResultMsg struct{ err error }
type channelsMsg struct {
	channels []*quorumv1.Channel
	err      error
}
type usersMsg struct {
	users []*quorumv1.User
	err   error
}
type historyMsg struct {
	convKey  string
	messages []*quorumv1.ChannelMessage
	prepend  bool // true: an older page to prepend; false: initial page to replace
	err      error
}
type actionErrMsg struct {
	context string
	err     error
}
type joinedMsg struct{ ch *quorumv1.Channel }

// pwResultMsg carries the outcome of a ChangePassword RPC back to the modal.
type pwResultMsg struct{ err error }

type focusArea int

const (
	focusInput focusArea = iota
	focusChannels
	focusDMs
)

type conversation struct {
	key           string // "ch:<id>" or "dm:<peerID>"
	id            string // channel ID or peer user ID
	name          string
	isDM          bool
	joined        bool // channels: membership
	online        bool // DMs: peer presence
	unread        int
	msgs          []message
	historyLoaded bool

	// Channel history pagination (scroll up to load older messages).
	oldestID     int64 // server id of the oldest loaded message; the next page's cursor
	hasMore      bool  // older messages may still exist on the server
	loadingOlder bool  // an older-history fetch is in flight

	// DM session state
	established bool
	fingerprint string
}

// msgKind selects how a scrollback entry is rendered.
type msgKind uint8

const (
	kindChat   msgKind = iota // a person's message: timestamp, sender, body
	kindSystem                // join/leave/notice, dimmed
	kindOK                    // a positive notice (e.g. session established), green
	kindError                 // a warning/error, red
	kindHelp                  // /help output, dimmed and ungutter­ed
)

// message is one entry in a conversation's scrollback. Entries are stored
// structured rather than pre-formatted so the view can colour senders, dim
// timestamps, and re-wrap to the current width on every render.
type message struct {
	ts     string // "15:04"; empty for non-chat lines
	sender string
	body   string
	kind   msgKind
	own    bool // true when the local user sent it
}

func chatLine(ts, sender, body string, own bool) message {
	return message{ts: ts, sender: sender, body: body, kind: kindChat, own: own}
}
func sysLine(body string) message  { return message{body: body, kind: kindSystem} }
func okLine(body string) message   { return message{body: body, kind: kindOK} }
func errLine(body string) message  { return message{body: body, kind: kindError} }
func helpLine(body string) message { return message{body: body, kind: kindHelp} }

func chKey(id string) string { return "ch:" + id }
func dmKey(id string) string { return "dm:" + id }

// Model is the root TUI model.
type Model struct {
	client *client.Client
	send   func(tea.Msg) // p.Send, installed by main before Run

	// login screen
	loggedIn   bool
	inputs     []textinput.Model // username, password
	loginFocus int
	loginErr   string
	loggingIn  bool

	// main screen
	width, height int
	focus         focusArea
	input         textinput.Model
	vp            viewport.Model
	convs         map[string]*conversation

	// Sidebar panels: channels and DMs are tracked, navigated, and scrolled
	// independently. Each panel keeps its own ordered key list, selection
	// cursor, and scroll offset (the index of the first visible row).
	chOrder, dmOrder   []string
	chIdx, dmIdx       int
	chScroll, dmScroll int

	activeKey   string
	connState   client.ConnState
	statusNote  string
	pendingJoin string                    // channel name a /join is waiting on a refresh for
	users       map[string]*quorumv1.User // by ID
	pumpStarted bool

	// pw is the modal password-change form; nil unless /passwd is open. The
	// form captures input out-of-band so a password is never typed into the
	// message line or echoed into the scrollback.
	pw *pwForm
}

// pwForm is the modal /passwd form: current, new, and confirm fields.
type pwForm struct {
	inputs []textinput.Model // current, new, confirm
	focus  int
	err    string
	busy   bool
}

func New(c *client.Client) *Model {
	username := textinput.New()
	username.Placeholder = "username"
	username.Focus()
	password := textinput.New()
	password.Placeholder = "password"
	password.EchoMode = textinput.EchoPassword

	input := textinput.New()
	input.Placeholder = "type a message, or /help for commands"
	input.CharLimit = 4000
	input.Prompt = "" // the content pane draws its own styled "› " prompt

	return &Model{
		client:    c,
		inputs:    []textinput.Model{username, password},
		input:     input,
		convs:     make(map[string]*conversation),
		users:     make(map[string]*quorumv1.User),
		connState: client.ConnOffline,
	}
}

// SetSend installs the program's Send function (used to pump gRPC events
// into the Update loop from outside).
func (m *Model) SetSend(send func(tea.Msg)) { m.send = send }

func (m *Model) Init() tea.Cmd { return textinput.Blink }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp = viewport.New(m.contentWidth(), m.contentHeight())
		m.input.Width = m.contentWidth() - 4
		m.ensureVisible()
		m.refreshViewport()
		return m, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
	}
	if !m.loggedIn {
		return m.updateLogin(msg)
	}
	return m.updateMain(msg)
}

func (m *Model) contentWidth() int  { return max(20, m.width-sidebarWidth-3) }
func (m *Model) contentHeight() int { return max(5, m.height-4) }

// sidebarHeight matches the content column's height exactly (header + viewport
// + input), so the two columns line up and the sidebar is clipped rather than
// allowed to push the body taller than the screen.
func (m *Model) sidebarHeight() int { return m.contentHeight() + 2 }

// listHeights splits the sidebar's vertical space between the channels and DMs
// panels. The sidebar spends three lines on chrome: a header above each list
// and a blank separator between the two.
func (m *Model) listHeights() (ch, dm int) {
	avail := m.sidebarHeight() - 3
	if avail < 2 {
		return 1, 1
	}
	ch = avail / 2
	return ch, avail - ch
}

// ensureVisible nudges each panel's scroll offset so its selection cursor stays
// on screen. Called after the cursor moves or the window resizes.
func (m *Model) ensureVisible() {
	chListH, dmListH := m.listHeights()
	m.chScroll = scrollToShow(m.chIdx, m.chScroll, chListH, len(m.chOrder))
	m.dmScroll = scrollToShow(m.dmIdx, m.dmScroll, dmListH, len(m.dmOrder))
}

// scrollToShow returns a scroll offset that keeps sel within the listH-row
// window, then clamps it to the valid range.
func scrollToShow(sel, scroll, listH, n int) int {
	if listH <= 0 || n == 0 {
		return 0
	}
	if sel < scroll {
		scroll = sel
	}
	if sel >= scroll+listH {
		scroll = sel - listH + 1
	}
	return clampScroll(scroll, listH, n)
}

// clampScroll bounds a scroll offset to [0, max(0, n-listH)] so a list never
// scrolls past its last item.
func clampScroll(scroll, listH, n int) int {
	maxScroll := max(0, n-listH)
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	return scroll
}

// --- login screen ---

func (m *Model) updateLogin(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyTab, tea.KeyShiftTab, tea.KeyDown, tea.KeyUp:
			m.loginFocus = (m.loginFocus + 1) % 2
			for i := range m.inputs {
				if i == m.loginFocus {
					m.inputs[i].Focus()
				} else {
					m.inputs[i].Blur()
				}
			}
			return m, nil
		case tea.KeyEnter:
			if m.loggingIn {
				return m, nil
			}
			username := strings.TrimSpace(m.inputs[0].Value())
			password := m.inputs[1].Value()
			if username == "" || password == "" {
				m.loginErr = "username and password required"
				return m, nil
			}
			m.loggingIn = true
			m.loginErr = ""
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				return loginResultMsg{err: m.client.Login(ctx, username, password)}
			}
		}
	case loginResultMsg:
		m.loggingIn = false
		if msg.err != nil {
			m.loginErr = grpcErrText(msg.err)
			return m, nil
		}
		m.loggedIn = true
		m.focus = focusInput
		m.input.Focus()
		return m, m.startPump()
	}
	var cmds []tea.Cmd
	for i := range m.inputs {
		var cmd tea.Cmd
		m.inputs[i], cmd = m.inputs[i].Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// startPump launches the client event pump exactly once; events arrive
// back as EventMsg via p.Send.
func (m *Model) startPump() tea.Cmd {
	if m.pumpStarted || m.send == nil {
		return nil
	}
	m.pumpStarted = true
	return func() tea.Msg {
		go m.client.Run(context.Background(), func(ev client.Event) {
			m.send(EventMsg{Ev: ev})
		})
		return nil
	}
}

// --- data refresh ---

func (m *Model) fetchChannels() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		chans, err := m.client.ListChannels(ctx)
		return channelsMsg{channels: chans, err: err}
	}
}

func (m *Model) fetchUsers() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		users, err := m.client.ListUsers(ctx)
		return usersMsg{users: users, err: err}
	}
}

func (m *Model) fetchHistory(conv *conversation) tea.Cmd {
	channelID, key := conv.id, conv.key
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		msgs, err := m.client.ChannelHistory(ctx, channelID, 0, historyPageSize)
		return historyMsg{convKey: key, messages: msgs, err: err}
	}
}

// fetchOlderHistory loads the page of messages immediately before the oldest one
// currently held, for scroll-up pagination.
func (m *Model) fetchOlderHistory(conv *conversation) tea.Cmd {
	channelID, key, before := conv.id, conv.key, conv.oldestID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		msgs, err := m.client.ChannelHistory(ctx, channelID, before, historyPageSize)
		return historyMsg{convKey: key, messages: msgs, prepend: true, err: err}
	}
}

// maybeLoadOlder kicks off an older-history fetch when the active channel's
// scrollback is near the top and more history may exist. It is a no-op for DMs
// (which have no server-side history), while a fetch is already in flight, or
// once the server has no older messages.
func (m *Model) maybeLoadOlder() tea.Cmd {
	conv := m.active()
	if conv == nil || conv.isDM || !conv.historyLoaded || !conv.hasMore || conv.loadingOlder {
		return nil
	}
	if m.vp.YOffset > historyScrollThreshold {
		return nil
	}
	conv.loadingOlder = true
	return m.fetchOlderHistory(conv)
}

// applyInitialHistory replaces a conversation's scrollback with a freshly loaded
// page and resets its pagination cursor.
func (m *Model) applyInitialHistory(conv *conversation, msgs []*quorumv1.ChannelMessage) {
	conv.loadingOlder = false // a reload (e.g. after reconnect) supersedes any older-page fetch
	conv.msgs = nil
	for _, cm := range msgs {
		conv.msgs = append(conv.msgs, chatLine(fmtTime(cm), cm.GetSenderName(), cm.GetBody(), m.isSelf(cm.GetSenderName())))
	}
	if len(msgs) > 0 {
		conv.oldestID = msgs[0].GetId()
	}
	conv.hasMore = len(msgs) == historyPageSize
	if conv.key == m.activeKey {
		m.refreshViewport()
	}
}

// applyOlderHistory prepends an older page of messages to a conversation,
// advancing the pagination cursor and keeping the viewport anchored on the
// message the user was reading.
func (m *Model) applyOlderHistory(conv *conversation, msgs []*quorumv1.ChannelMessage) {
	if len(msgs) == 0 {
		conv.hasMore = false
		return
	}
	older := make([]message, 0, len(msgs))
	for _, cm := range msgs {
		older = append(older, chatLine(fmtTime(cm), cm.GetSenderName(), cm.GetBody(), m.isSelf(cm.GetSenderName())))
	}
	conv.msgs = append(older, conv.msgs...)
	conv.oldestID = msgs[0].GetId()
	conv.hasMore = len(msgs) == historyPageSize
	if conv.key == m.activeKey {
		m.refreshViewportKeepingScroll()
	}
}

// ensureConv returns the conversation, creating it if new.
func (m *Model) ensureConv(key, id, name string, isDM bool) *conversation {
	if c, ok := m.convs[key]; ok {
		return c
	}
	c := &conversation{key: key, id: id, name: name, isDM: isDM}
	if isDM {
		// Seed presence from the directory. An incoming DM can create the
		// conversation for a peer who is already online and will therefore
		// send no further presence event to flip the indicator on.
		if u, ok := m.users[id]; ok {
			c.online = u.GetOnline()
		}
	}
	m.convs[key] = c
	m.rebuildOrder()
	return c
}

func (m *Model) rebuildOrder() {
	var chs, dms []string
	for key, c := range m.convs {
		if c.isDM {
			dms = append(dms, key)
		} else {
			chs = append(chs, key)
		}
	}
	byName := func(keys []string) {
		sort.Slice(keys, func(i, j int) bool { return m.convs[keys[i]].name < m.convs[keys[j]].name })
	}
	byName(chs)
	byName(dms)
	m.chOrder, m.dmOrder = chs, dms
	if m.chIdx >= len(m.chOrder) {
		m.chIdx = max(0, len(m.chOrder)-1)
	}
	if m.dmIdx >= len(m.dmOrder) {
		m.dmIdx = max(0, len(m.dmOrder)-1)
	}
	m.ensureVisible()
}

func (m *Model) active() *conversation {
	if m.activeKey == "" {
		return nil
	}
	return m.convs[m.activeKey]
}

// isSelf reports whether a sender name belongs to the local user, used to give
// the user's own messages a distinct colour in the scrollback.
func (m *Model) isSelf(sender string) bool {
	return m.client != nil && sender == m.client.Username()
}

// push appends an entry to a conversation's scrollback, capping its length.
// The active conversation re-renders; others bump their unread badge.
func (m *Model) push(conv *conversation, msg message) {
	conv.msgs = append(conv.msgs, msg)
	if len(conv.msgs) > 2000 {
		conv.msgs = conv.msgs[len(conv.msgs)-2000:]
	}
	if conv.key == m.activeKey {
		m.refreshViewport()
	} else {
		conv.unread++
	}
}

// renderConv renders a conversation's scrollback into viewport content, wrapped
// to the current content width.
func (m *Model) renderConv(conv *conversation) string {
	w := m.contentWidth()
	rendered := make([]string, len(conv.msgs))
	for i, msg := range conv.msgs {
		rendered[i] = renderMessage(msg, w)
	}
	return strings.Join(rendered, "\n")
}

func (m *Model) refreshViewport() {
	conv := m.active()
	if conv == nil {
		m.vp.SetContent(m.welcomeView())
		return
	}
	m.vp.SetContent(m.renderConv(conv))
	m.vp.GotoBottom()
}

// refreshViewportKeepingScroll re-renders the active conversation after older
// messages were prepended, shifting the scroll offset down by the number of new
// lines so the message the user was reading stays put instead of jumping.
func (m *Model) refreshViewportKeepingScroll() {
	conv := m.active()
	if conv == nil {
		return
	}
	oldOffset := m.vp.YOffset
	oldTotal := m.vp.TotalLineCount()
	m.vp.SetContent(m.renderConv(conv))
	m.vp.SetYOffset(oldOffset + m.vp.TotalLineCount() - oldTotal)
}

func fmtTime(ts *quorumv1.ChannelMessage) string {
	if ts.GetSentAt() == nil {
		return time.Now().Format("15:04")
	}
	return ts.GetSentAt().AsTime().Local().Format("15:04")
}

func grpcErrText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	// Strip the gRPC prefix for display.
	if _, after, found := strings.Cut(s, "desc = "); found {
		return after
	}
	return s
}
