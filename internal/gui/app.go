// Package gui implements the Fyne desktop chat client. It is a graphical
// peer of internal/tui/chat: both drive the same internal/client core (TLS
// dialing, login, the Subscribe event pump, and the E2EE DM manager). All
// conversation state is mutated only on the Fyne UI goroutine; background
// work (RPCs, the event pump) marshals back through fyne.Do.
package gui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	quorumv1 "github.com/layer8/quorum/gen/quorum/v1"
	"github.com/layer8/quorum/internal/client"
)

// Defaults seed the connection form (from command-line flags).
type Defaults struct {
	Addr    string
	CAFile  string
	DataDir string
}

// App is the root of the GUI client.
type App struct {
	fyneApp  fyne.App
	win      fyne.Window
	defaults Defaults

	client *client.Client

	// conversation state (UI goroutine only)
	convs     map[string]*conversation
	chOrder   []string
	dmOrder   []string
	activeKey string
	users     map[string]*quorumv1.User
	connState client.ConnState

	pendingJoin string // channel name a /join is waiting on a refresh for
	note        string // transient status-bar note
	selecting   bool   // guards programmatic list selection from re-entering openConv
	pumpStarted bool

	// main-window widgets (nil until showMain)
	channelList *widget.List
	dmList      *widget.List
	msgBox      *fyne.Container
	msgScroll   *container.Scroll
	input       *widget.Entry
	header      *widget.Label
	status      *widget.Label
}

// NewApp builds the Fyne app and window, seeded with connection defaults.
func NewApp(d Defaults) *App {
	fa := fyneapp.NewWithID("org.layer8.quorum")
	a := &App{
		fyneApp:   fa,
		defaults:  d,
		convs:     make(map[string]*conversation),
		users:     make(map[string]*quorumv1.User),
		connState: client.ConnOffline,
	}
	a.win = fa.NewWindow("Quorum")
	a.win.Resize(fyne.NewSize(960, 640))
	return a
}

// Run shows the login screen and enters the Fyne event loop (blocks).
func (a *App) Run() {
	a.win.SetContent(a.buildLogin())
	a.win.ShowAndRun()
}

// contextWithTimeout is the standard short-lived RPC context used for
// interactive login and dialing.
func contextWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 20*time.Second)
}

// dialAndLogin connects to the server and authenticates, closing the
// connection if login fails so a failed attempt leaks nothing.
func dialAndLogin(ctx context.Context, addr, caFile, dataDir, username, password string) (*client.Client, error) {
	c, err := client.Dial(client.Config{Addr: addr, CAFile: caFile, DataDir: dataDir})
	if err != nil {
		return nil, err
	}
	if err := c.Login(ctx, username, password); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// showMain swaps the window to the chat UI and starts the event pump.
func (a *App) showMain() {
	a.win.SetContent(a.buildMain())
	a.win.SetTitle("Quorum — " + a.client.Username())
	a.win.Canvas().Focus(a.input)
	a.updateStatus()
	a.startPump()
}

// startPump launches the client event pump exactly once; events are marshalled
// onto the UI goroutine before touching widgets.
func (a *App) startPump() {
	if a.pumpStarted {
		return
	}
	a.pumpStarted = true
	go a.client.Run(context.Background(), func(ev client.Event) {
		fyne.Do(func() { a.handleEvent(ev) })
	})
}

// --- conversation bookkeeping ---

// ensureConv returns the conversation for key, creating it if new.
func (a *App) ensureConv(key, id, name string, isDM bool) *conversation {
	if c, ok := a.convs[key]; ok {
		return c
	}
	c := &conversation{key: key, id: id, name: name, isDM: isDM}
	if isDM {
		// Seed presence from the directory: an incoming DM can create the
		// conversation for a peer who is already online and will send no
		// further presence event.
		if u, ok := a.users[id]; ok {
			c.online = u.GetOnline()
		}
	}
	a.convs[key] = c
	a.rebuildOrder()
	return c
}

// rebuildOrder re-derives the two sorted sidebar key lists and refreshes them.
func (a *App) rebuildOrder() {
	var chs, dms []string
	for key, c := range a.convs {
		if c.isDM {
			dms = append(dms, key)
		} else {
			chs = append(chs, key)
		}
	}
	byName := func(keys []string) {
		sort.Slice(keys, func(i, j int) bool { return a.convs[keys[i]].name < a.convs[keys[j]].name })
	}
	byName(chs)
	byName(dms)
	a.chOrder, a.dmOrder = chs, dms
	a.refreshLists()
}

func (a *App) active() *conversation {
	if a.activeKey == "" {
		return nil
	}
	return a.convs[a.activeKey]
}

func (a *App) isSelf(sender string) bool {
	return a.client != nil && sender == a.client.Username()
}

// push appends an entry to a conversation's scrollback (capped). The active
// conversation gets a new rendered row; others bump their unread badge.
func (a *App) push(conv *conversation, msg message) {
	conv.msgs = append(conv.msgs, msg)
	if len(conv.msgs) > 2000 {
		conv.msgs = conv.msgs[len(conv.msgs)-2000:]
	}
	if conv.key == a.activeKey {
		a.msgBox.Add(messageRow(msg))
		a.msgScroll.ScrollToBottom()
	} else {
		conv.unread++
		a.refreshListFor(conv)
	}
}

// findChannelKey returns the conversation key for a channel matching name
// (with or without a leading '#'), compared case-insensitively, or "".
func (a *App) findChannelKey(name string) string {
	name = strings.TrimPrefix(name, "#")
	for key, conv := range a.convs {
		if !conv.isDM && strings.EqualFold(strings.TrimPrefix(conv.name, "#"), name) {
			return key
		}
	}
	return ""
}

// --- navigation ---

// openConv activates a sidebar entry, joining a channel first if needed.
func (a *App) openConv(key string) {
	conv := a.convs[key]
	if conv == nil {
		return
	}
	if !conv.isDM && !conv.joined {
		channelID := conv.id
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ch, err := a.client.JoinChannel(ctx, channelID)
			fyne.Do(func() {
				if err != nil {
					a.setStatus("join: " + grpcErrText(err))
					return
				}
				c := a.ensureConv(chKey(ch.GetId()), ch.GetId(), "#"+ch.GetName(), false)
				c.joined = true
				a.setActive(c.key)
				if !c.historyLoaded {
					c.historyLoaded = true
					a.fetchHistory(c)
				}
			})
		}()
		return
	}
	a.setActive(key)
	if !conv.isDM && !conv.historyLoaded {
		conv.historyLoaded = true
		a.fetchHistory(conv)
	}
}

// setActive switches the message pane to key, clears its unread badge, and
// syncs the sidebar selection, header, and scrollback.
func (a *App) setActive(key string) {
	a.activeKey = key
	conv := a.convs[key]
	if conv != nil {
		conv.unread = 0
	}
	a.syncSelection(key)
	a.updateHeader()
	a.refreshLists()
	if conv != nil {
		a.rebuildMessages(conv)
	} else {
		a.clearMessages()
	}
	a.win.Canvas().Focus(a.input)
}

// syncSelection highlights key's row in its panel and clears the other panel's
// selection, guarding against re-entering openConv via the OnSelected callback.
func (a *App) syncSelection(key string) {
	a.selecting = true
	defer func() { a.selecting = false }()
	if conv := a.convs[key]; conv != nil && conv.isDM {
		a.channelList.UnselectAll()
		for i, k := range a.dmOrder {
			if k == key {
				a.dmList.Select(i)
				return
			}
		}
		return
	}
	a.dmList.UnselectAll()
	for i, k := range a.chOrder {
		if k == key {
			a.channelList.Select(i)
			return
		}
	}
}

func (a *App) openDM(username string) {
	username = strings.TrimPrefix(username, "@")
	for _, u := range a.users {
		if strings.EqualFold(u.GetUsername(), username) {
			if u.GetId() == a.client.UserID() {
				a.setStatus("that's you")
				return
			}
			conv := a.ensureConv(dmKey(u.GetId()), u.GetId(), "@"+u.GetUsername(), true)
			a.setActive(conv.key)
			return
		}
	}
	a.setStatus("unknown user " + username)
	a.fetchUsers()
}

// --- outbound actions ---

// submit handles the input line: local slash commands or a message send. It
// mirrors the TUI's command set so the two clients behave identically.
func (a *App) submit(text string) {
	text = strings.TrimSpace(text)
	a.input.SetText("")
	if text == "" {
		return
	}
	if strings.HasPrefix(text, "/") {
		fields := strings.Fields(text)
		switch fields[0] {
		case "/help":
			a.showHelp()
			return
		case "/quit":
			a.fyneApp.Quit()
			return
		case "/create":
			if len(fields) < 2 {
				a.setStatus("usage: /create <name>")
				return
			}
			a.createChannel(fields[1])
			return
		case "/join":
			if len(fields) < 2 {
				a.setStatus("usage: /join <name>")
				return
			}
			target := strings.TrimPrefix(fields[1], "#")
			if key := a.findChannelKey(target); key != "" {
				a.openConv(key)
				return
			}
			// The local list may be stale: refresh and finish the join when it
			// arrives (see applyChannels).
			a.pendingJoin = target
			a.setStatus("looking for #" + target + "…")
			a.fetchChannels()
			return
		case "/leave":
			a.leaveActive()
			return
		case "/dm":
			if len(fields) < 2 {
				a.setStatus("usage: /dm <username>")
				return
			}
			a.openDM(fields[1])
			return
		}
		// Unknown slash commands (a bot's own command, or /commands) fall
		// through to the channel so bots and the server can see them.
	}

	conv := a.active()
	if conv == nil {
		a.setStatus("select a conversation first")
		return
	}
	if conv.isDM {
		peerID, peerName := conv.id, strings.TrimPrefix(conv.name, "@")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := a.client.SendDM(ctx, peerID, peerName, text); err != nil {
				fyne.Do(func() { a.setStatus("dm: " + grpcErrText(err)) })
			}
		}()
		return
	}
	channelID := conv.id
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.client.SendChannelMessage(ctx, channelID, text); err != nil {
			fyne.Do(func() { a.setStatus("send: " + grpcErrText(err)) })
		}
	}()
}

// createChannel creates a channel and opens it on success.
func (a *App) createChannel(name string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ch, err := a.client.CreateChannel(ctx, name)
		fyne.Do(func() {
			if err != nil {
				a.setStatus("create: " + grpcErrText(err))
				return
			}
			conv := a.ensureConv(chKey(ch.GetId()), ch.GetId(), "#"+ch.GetName(), false)
			conv.joined = true
			a.setActive(conv.key)
			if !conv.historyLoaded {
				conv.historyLoaded = true
				a.fetchHistory(conv)
			}
		})
	}()
}

// leaveActive leaves the active channel, removing it from the sidebar.
func (a *App) leaveActive() {
	conv := a.active()
	if conv == nil || conv.isDM {
		a.setStatus("/leave works in a channel")
		return
	}
	channelID, key := conv.id, conv.key
	delete(a.convs, key)
	a.activeKey = ""
	a.rebuildOrder()
	a.clearMessages()
	a.updateHeader()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.client.LeaveChannel(ctx, channelID); err != nil {
			fyne.Do(func() { a.setStatus("leave: " + grpcErrText(err)) })
		}
	}()
}

// --- data refresh (each spawns a goroutine and marshals results back) ---

func (a *App) fetchChannels() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		chans, err := a.client.ListChannels(ctx)
		fyne.Do(func() { a.applyChannels(chans, err) })
	}()
}

func (a *App) applyChannels(chans []*quorumv1.Channel, err error) {
	if err != nil {
		a.setStatus("channel list: " + grpcErrText(err))
		return
	}
	for _, ch := range chans {
		conv := a.ensureConv(chKey(ch.GetId()), ch.GetId(), "#"+ch.GetName(), false)
		conv.joined = ch.GetIsMember()
		if conv.joined && !conv.historyLoaded {
			conv.historyLoaded = true
			a.fetchHistory(conv)
		}
	}
	a.rebuildOrder()
	if a.pendingJoin != "" {
		name := a.pendingJoin
		a.pendingJoin = ""
		if key := a.findChannelKey(name); key != "" {
			a.openConv(key)
		} else {
			a.setStatus("no such channel #" + name)
		}
	}
}

func (a *App) fetchUsers() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		users, err := a.client.ListUsers(ctx)
		fyne.Do(func() { a.applyUsers(users, err) })
	}()
}

func (a *App) applyUsers(users []*quorumv1.User, err error) {
	if err != nil {
		return
	}
	// Surface the whole directory in the DMs panel so any user can be opened,
	// not just peers already messaged. Skip yourself and bots: bots publish no
	// identity key, so there is no E2EE session to open with them.
	for _, u := range users {
		a.users[u.GetId()] = u
		if u.GetId() == a.client.UserID() || u.GetRole() == "bot" {
			continue
		}
		conv := a.ensureConv(dmKey(u.GetId()), u.GetId(), "@"+u.GetUsername(), true)
		conv.online = u.GetOnline()
	}
	a.refreshLists()
}

func (a *App) fetchHistory(conv *conversation) {
	channelID, key := conv.id, conv.key
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		msgs, err := a.client.ChannelHistory(ctx, channelID, 0, 100)
		fyne.Do(func() { a.applyHistory(key, msgs, err) })
	}()
}

func (a *App) applyHistory(key string, msgs []*quorumv1.ChannelMessage, err error) {
	if err != nil {
		a.setStatus("history: " + grpcErrText(err))
		return
	}
	conv := a.convs[key]
	if conv == nil {
		return
	}
	conv.msgs = nil
	for _, cm := range msgs {
		conv.msgs = append(conv.msgs, chatLine(fmtTime(cm), cm.GetSenderName(), cm.GetBody(), a.isSelf(cm.GetSenderName())))
	}
	if key == a.activeKey {
		a.rebuildMessages(conv)
	}
}

// --- inbound events from the client pump (already on the UI goroutine) ---

func (a *App) handleEvent(ev client.Event) {
	switch ev := ev.(type) {
	case client.ConnStateEvent:
		a.connState = ev.State
		if ev.Err != nil {
			a.setStatus(grpcErrText(ev.Err))
		} else {
			a.updateStatus()
		}
	case client.ResyncEvent:
		for _, conv := range a.convs {
			if !conv.isDM {
				conv.historyLoaded = false
			}
		}
		a.fetchChannels()
		a.fetchUsers()
	case client.ChannelMessageEvent:
		cm := ev.Msg
		conv := a.ensureConv(chKey(cm.GetChannelId()), cm.GetChannelId(), "#"+cm.GetChannelId(), false)
		a.push(conv, chatLine(fmtTime(cm), cm.GetSenderName(), cm.GetBody(), a.isSelf(cm.GetSenderName())))
	case client.DirectMessageEvent:
		conv := a.ensureConv(dmKey(ev.PeerID), ev.PeerID, "@"+ev.PeerName, true)
		who := ev.PeerName
		if ev.Outgoing {
			who = a.client.Username()
		}
		a.push(conv, chatLine(time.Now().Format("15:04"), who, ev.Text, ev.Outgoing))
	case client.DMSessionEvent:
		conv := a.ensureConv(dmKey(ev.PeerID), ev.PeerID, "@"+ev.PeerName, true)
		conv.established = ev.Established
		if ev.Fingerprint != "" {
			conv.fingerprint = ev.Fingerprint
		}
		switch {
		case ev.Err != nil:
			a.push(conv, errLine(ev.Err.Error()))
		case ev.Established:
			a.push(conv, okLine(fmt.Sprintf("🔒 encrypted session established — their key: %s", conv.fingerprint)))
		default:
			a.push(conv, sysLine("🔓 session closed"))
		}
		if conv.key == a.activeKey {
			a.updateHeader()
		}
	case client.PresenceEvent:
		p := ev.Presence
		u, known := a.users[p.GetUserId()]
		if !known {
			// A user we haven't seen just came online: pull the directory to add
			// them with the bot filter applied. Going-offline needs no action.
			if p.GetOnline() {
				a.fetchUsers()
			}
			return
		}
		u.Online = p.GetOnline()
		if conv, ok := a.convs[dmKey(p.GetUserId())]; ok {
			conv.online = p.GetOnline()
		}
		a.refreshListFor(&conversation{isDM: true})
	case client.ChannelEventEvent:
		ce := ev.Event
		ch := ce.GetChannel()
		conv := a.ensureConv(chKey(ch.GetId()), ch.GetId(), "#"+ch.GetName(), false)
		switch ce.GetType() {
		case 1: // created
		case 2:
			a.push(conv, sysLine(fmt.Sprintf("→ %s joined", ce.GetUsername())))
		case 3:
			a.push(conv, sysLine(fmt.Sprintf("← %s left", ce.GetUsername())))
		}
	case client.SystemEvent:
		if t := ev.Notice.GetText(); t != "" && t != "connected" {
			if conv := a.active(); conv != nil {
				a.push(conv, sysLine(t))
			} else {
				a.setStatus(t)
			}
		}
	case client.ErrorEvent:
		a.setStatus(ev.Context + ": " + ev.Err.Error())
	}
}
