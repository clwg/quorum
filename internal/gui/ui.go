package gui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// loginFormWidth is the fixed width of the centred connection form.
const loginFormWidth = 420

// formDialogWidth widens the modal entry dialogs beyond their natural
// label-hugging width so the entry fields are comfortable to type in.
const formDialogWidth = 480

// searchDialogHeight is the fixed height of the /search results dialog: tall
// enough to show several matches at once while the inner list scrolls the rest.
const searchDialogHeight = 460

// buildLogin constructs the connection/login screen. Dialing and login run off
// the UI goroutine; on success the window swaps to the chat UI.
func (a *App) buildLogin() fyne.CanvasObject {
	addr := widget.NewEntry()
	addr.SetText(a.defaults.Addr)
	addr.SetPlaceHolder("localhost:8443")

	ca := widget.NewEntry()
	ca.SetText(a.defaults.CAFile)
	ca.SetPlaceHolder("path to CA cert (optional)")

	user := widget.NewEntry()
	user.SetPlaceHolder("username")

	pass := widget.NewPasswordEntry()
	pass.SetPlaceHolder("password")

	status := widget.NewLabel("")
	status.Wrapping = fyne.TextWrapWord

	var connectBtn *widget.Button
	connect := func() {
		username := strings.TrimSpace(user.Text)
		password := pass.Text
		address := strings.TrimSpace(addr.Text)
		caFile := strings.TrimSpace(ca.Text)
		if address == "" || username == "" || password == "" {
			status.SetText("server address, username and password are required")
			return
		}
		connectBtn.Disable()
		status.SetText("connecting…")
		go func() {
			ctx, cancel := contextWithTimeout()
			defer cancel()
			c, err := dialAndLogin(ctx, address, caFile, a.defaults.DataDir, username, password)
			fyne.Do(func() {
				if err != nil {
					connectBtn.Enable()
					status.SetText(grpcErrText(err))
					return
				}
				a.client = c
				a.showMain()
			})
		}()
	}
	pass.OnSubmitted = func(string) { connect() }
	connectBtn = widget.NewButton("Connect", connect)
	connectBtn.Importance = widget.HighImportance

	form := widget.NewForm(
		widget.NewFormItem("Server", addr),
		widget.NewFormItem("CA cert", ca),
		widget.NewFormItem("Username", user),
		widget.NewFormItem("Password", pass),
	)

	card := container.NewVBox(
		widget.NewLabelWithStyle("Quorum", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		widget.NewLabel(""),
		form,
		connectBtn,
		status,
	)
	return container.NewCenter(container.NewGridWrap(fyne.NewSize(loginFormWidth, card.MinSize().Height), card))
}

// buildMain constructs the chat UI: a channels/DMs sidebar, a message pane with
// header and input, and a bottom status bar.
func (a *App) buildMain() fyne.CanvasObject {
	a.channelList = widget.NewList(
		func() int { return len(a.chOrder) },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(id widget.ListItemID, o fyne.CanvasObject) {
			if id >= 0 && id < len(a.chOrder) {
				o.(*widget.Label).SetText(a.channelRowText(a.chOrder[id]))
			}
		},
	)
	a.channelList.OnSelected = func(id widget.ListItemID) {
		if a.selecting || id < 0 || id >= len(a.chOrder) {
			return
		}
		a.openConv(a.chOrder[id])
	}

	a.dmList = widget.NewList(
		func() int { return len(a.dmOrder) },
		func() fyne.CanvasObject {
			// The template must be a full-height representative row so the list
			// derives the correct item height from it.
			rt := widget.NewRichText(
				&widget.TextSegment{Text: "● ", Style: widget.RichTextStyle{Inline: true}},
				&widget.TextSegment{Text: "@user", Style: widget.RichTextStyle{Inline: true}},
			)
			rt.Wrapping = fyne.TextWrapOff
			return rt
		},
		func(id widget.ListItemID, o fyne.CanvasObject) {
			if id < 0 || id >= len(a.dmOrder) {
				return
			}
			if c := a.convs[a.dmOrder[id]]; c != nil {
				rt := o.(*widget.RichText)
				rt.Segments = dmRowSegments(c)
				rt.Refresh()
			}
		},
	)
	a.dmList.OnSelected = func(id widget.ListItemID) {
		if a.selecting || id < 0 || id >= len(a.dmOrder) {
			return
		}
		a.openConv(a.dmOrder[id])
	}

	addBtn := widget.NewButtonWithIcon("", theme.ContentAddIcon(), a.promptCreateChannel)
	addBtn.Importance = widget.LowImportance
	chHeader := container.NewBorder(nil, nil, nil, addBtn,
		widget.NewLabelWithStyle("CHANNELS", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
	dmHeader := widget.NewLabelWithStyle("DIRECT MESSAGES", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})

	sidebar := container.NewVSplit(
		container.NewBorder(chHeader, nil, nil, nil, a.channelList),
		container.NewBorder(dmHeader, nil, nil, nil, a.dmList),
	)
	sidebar.SetOffset(0.5)

	a.header = widget.NewLabel("Select a conversation")
	a.header.TextStyle = fyne.TextStyle{Bold: true}

	a.msgBox = container.NewVBox()
	a.msgScroll = container.NewVScroll(a.msgBox)
	a.msgScroll.OnScrolled = func(pos fyne.Position) { a.maybeLoadOlder(pos) }

	a.input = widget.NewEntry()
	a.input.SetPlaceHolder("Type a message, or /help for commands")
	a.input.OnSubmitted = func(s string) { a.submit(s) }
	sendBtn := widget.NewButtonWithIcon("", theme.MailSendIcon(), func() { a.submit(a.input.Text) })
	inputBar := container.NewBorder(nil, nil, nil, sendBtn, a.input)

	pane := container.NewBorder(
		container.NewVBox(a.header, widget.NewSeparator()),
		inputBar, nil, nil,
		a.msgScroll,
	)

	a.status = widget.NewLabel("")
	statusBar := container.NewVBox(widget.NewSeparator(), a.status)

	split := container.NewHSplit(sidebar, pane)
	split.SetOffset(0.26)

	return container.NewBorder(nil, statusBar, nil, nil, split)
}

// channelRowText renders a channel's sidebar row: the name, a "(join)" hint
// for channels not yet joined, and an unread badge.
func (a *App) channelRowText(key string) string {
	c := a.convs[key]
	if c == nil {
		return ""
	}
	name := c.name
	if !c.joined {
		name += "  (join)"
	}
	if c.unread > 0 {
		name = fmt.Sprintf("%s  (%d)", name, c.unread)
	}
	return name
}

// dmRowSegments renders a DM's sidebar row: a presence bullet - green and
// filled when the peer is online, hollow otherwise - the name, and an unread
// badge.
func dmRowSegments(c *conversation) []widget.RichTextSegment {
	bullet, bulletColor := "○ ", theme.ColorNameForeground
	if c.online {
		bullet, bulletColor = "● ", theme.ColorNameSuccess
	}
	name := c.name
	if c.unread > 0 {
		name = fmt.Sprintf("%s  (%d)", name, c.unread)
	}
	return []widget.RichTextSegment{
		&widget.TextSegment{Text: bullet, Style: widget.RichTextStyle{Inline: true, ColorName: bulletColor}},
		&widget.TextSegment{Text: name, Style: widget.RichTextStyle{Inline: true}},
	}
}

// refreshLists redraws both sidebar lists (no-op before buildMain).
func (a *App) refreshLists() {
	if a.channelList != nil {
		a.channelList.Refresh()
	}
	if a.dmList != nil {
		a.dmList.Refresh()
	}
}

// refreshListFor redraws the sidebar list that owns conv.
func (a *App) refreshListFor(conv *conversation) {
	if conv.isDM {
		if a.dmList != nil {
			a.dmList.Refresh()
		}
		return
	}
	if a.channelList != nil {
		a.channelList.Refresh()
	}
}

// rebuildMessages repopulates the message pane from a conversation's scrollback.
func (a *App) rebuildMessages(conv *conversation) {
	objs := make([]fyne.CanvasObject, 0, len(conv.msgs))
	for _, m := range conv.msgs {
		objs = append(objs, messageRow(m))
	}
	a.msgBox.Objects = objs
	a.msgBox.Refresh()
	a.scrollToBottom()
}

// layoutContent forces the message box to lay out at the current viewport width
// and sizes it to fit, returning the resulting content height. This mirrors what
// the Scroll's renderer does (content.Resize(MinSize().Max(viewport))) but only
// on the next canvas pass - too late for the scroll math we run immediately after
// changing the rows. Container.Resize lays out children synchronously, so doing
// it here makes word-wrapped rows report their true height and grows the content
// past the viewport, which is what Scroll.updateOffset checks before honouring a
// scroll (otherwise it snaps the offset back to the top). Returns 0 before the
// pane has a size (e.g. the very first load, before the window is laid out).
func (a *App) layoutContent() float32 {
	viewport := a.msgScroll.Size()
	if viewport.Height <= 0 {
		return 0
	}
	a.msgBox.Resize(fyne.NewSize(viewport.Width, viewport.Height)) // width: rows wrap
	full := a.msgBox.MinSize().Max(viewport)                       // height: real total
	a.msgBox.Resize(full)
	return full.Height
}

// scrollToBottom scrolls the message pane to the latest message. ScrollToBottom
// fires OnScrolled synchronously, so it is bracketed with suppressLoad to keep
// this programmatic jump from being mistaken for the user scrolling up.
func (a *App) scrollToBottom() {
	a.layoutContent()
	a.suppressLoad = true
	a.msgScroll.ScrollToBottom()
	a.suppressLoad = false
}

// prependMessageRows inserts freshly loaded older rows above the current ones,
// preserving the scroll position so the view stays anchored on the message the
// user was reading instead of jumping as the content grows upward.
func (a *App) prependMessageRows(older []message) {
	prevHeight := a.msgBox.MinSize().Height
	oldOffset := a.msgScroll.Offset
	rows := make([]fyne.CanvasObject, 0, len(older)+len(a.msgBox.Objects))
	for _, m := range older {
		rows = append(rows, messageRow(m))
	}
	rows = append(rows, a.msgBox.Objects...)
	a.msgBox.Objects = rows
	a.msgBox.Refresh()
	added := a.layoutContent() - prevHeight
	a.suppressLoad = true
	a.msgScroll.ScrollToOffset(fyne.NewPos(oldOffset.X, oldOffset.Y+added))
	a.suppressLoad = false
}

func (a *App) clearMessages() {
	a.msgBox.Objects = nil
	a.msgBox.Refresh()
}

// updateHeader sets the message-pane title to the active conversation, with
// the E2EE status for DMs.
func (a *App) updateHeader() {
	conv := a.active()
	if conv == nil {
		a.header.SetText("Select a conversation")
		return
	}
	if !conv.isDM {
		a.header.SetText(conv.name)
		return
	}
	if conv.established {
		a.header.SetText(fmt.Sprintf("%s    🔒 %s", conv.name, conv.fingerprint))
	} else {
		a.header.SetText(conv.name + "    🔓 not yet encrypted")
	}
}

// setStatus shows a transient note alongside the connection summary.
func (a *App) setStatus(note string) {
	a.note = note
	a.updateStatus()
}

// updateStatus rebuilds the bottom status bar: connection state, identity, and
// any transient note.
func (a *App) updateStatus() {
	if a.status == nil {
		return
	}
	parts := []string{a.connState.String()}
	if a.client != nil {
		if name := a.client.Username(); name != "" {
			parts = append(parts, name)
		}
		if fp := a.client.IdentityFingerprint(); fp != "" {
			parts = append(parts, "key "+fp)
		}
	}
	line := strings.Join(parts, "   •   ")
	if a.note != "" {
		line += "      " + a.note
	}
	a.status.SetText(line)
}

// promptCreateChannel asks for a channel name and creates it.
func (a *App) promptCreateChannel() {
	entry := widget.NewEntry()
	entry.SetPlaceHolder("name")
	d := dialog.NewForm("Create channel", "Create", "Cancel",
		[]*widget.FormItem{widget.NewFormItem("Channel", entry)},
		func(ok bool) {
			if !ok {
				return
			}
			if name := strings.TrimSpace(entry.Text); name != "" {
				a.createChannel(name)
			}
		}, a.win)
	// Widen past the natural label-hugging width (keeping the natural height) so
	// the name field has room.
	d.Resize(fyne.NewSize(formDialogWidth, d.MinSize().Height))
	d.Show()
}

// promptChangePassword opens a modal form to change the user's password. The
// three fields are masked; the new/confirm fields carry validators, so the
// dialog's "Change" button stays disabled until the new password is long enough
// and the confirmation matches. The server still verifies the current password
// and re-checks the rules.
func (a *App) promptChangePassword() {
	current := widget.NewPasswordEntry()
	next := widget.NewPasswordEntry()
	confirm := widget.NewPasswordEntry()

	next.Validator = func(s string) error {
		if len(s) < 8 {
			return errors.New("at least 8 characters")
		}
		return nil
	}
	confirm.Validator = func(s string) error {
		if s != next.Text {
			return errors.New("does not match")
		}
		return nil
	}
	// Re-validate the confirmation as the new password changes, so the match
	// check tracks edits to either field.
	next.OnChanged = func(string) { confirm.Validate() }

	items := []*widget.FormItem{
		widget.NewFormItem("Current", current),
		widget.NewFormItem("New", next),
		widget.NewFormItem("Confirm", confirm),
	}
	d := dialog.NewForm("Change password", "Change", "Cancel", items, func(ok bool) {
		if !ok {
			return
		}
		old, neu := current.Text, next.Text
		if old == "" {
			a.setStatus("change password: enter your current password")
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err := a.client.ChangePassword(ctx, old, neu)
			fyne.Do(func() {
				if err != nil {
					a.setStatus("change password: " + grpcErrText(err))
					return
				}
				a.setStatus("password changed")
			})
		}()
	}, a.win)
	// The form's natural width hugs the labels, leaving the password fields
	// cramped; widen it (keeping the natural height) so the entries have room.
	d.Resize(fyne.NewSize(formDialogWidth, d.MinSize().Height))
	d.Show()
}

// showHelp lists the client's slash commands in a dialog. Commands and their
// descriptions are laid out in a two-column form grid (the command monospaced),
// so the columns line up regardless of the theme's proportional font — unlike a
// single space-padded text block, which doesn't align.
func (a *App) showHelp() {
	commands := [][2]string{
		{"/create <name>", "create a channel and open it"},
		{"/join <name>", "join an existing channel"},
		{"/leave", "leave the current channel"},
		{"/dm <user>", "open an end-to-end-encrypted direct message"},
		{"/search <query>", "search this channel's history"},
		{"/passwd", "change your password (opens a private form)"},
		{"/commands", "list commands offered by bots"},
		{"/help", "show this help"},
		{"/quit", "exit quorum"},
	}
	grid := container.New(layout.NewFormLayout())
	for _, c := range commands {
		cmd := widget.NewLabelWithStyle(c[0], fyne.TextAlignLeading, fyne.TextStyle{Monospace: true})
		grid.Add(cmd)
		grid.Add(widget.NewLabel(c[1]))
	}

	tips := widget.NewLabel("Click a channel or user in the sidebar to open it.\n" +
		"The + above CHANNELS creates a new channel.")
	tips.Wrapping = fyne.TextWrapWord

	content := container.NewVBox(grid, widget.NewSeparator(), tips)
	d := dialog.NewCustom("Quorum commands", "Close", content, a.win)
	d.Resize(fyne.NewSize(formDialogWidth, d.MinSize().Height))
	d.Show()
}

// showSearchResults presents channel-search matches in a scrollable dialog.
// Each row shows the match's date, sender, and body with the query highlighted.
func (a *App) showSearchResults(query string, results []message) {
	rows := make([]fyne.CanvasObject, len(results))
	for i, m := range results {
		rows[i] = searchResultRow(m, query)
	}
	list := container.NewVScroll(container.NewVBox(rows...))
	matches := "matches"
	if len(results) == 1 {
		matches = "match"
	}
	title := fmt.Sprintf("Search: %q  (%d %s)", query, len(results), matches)
	d := dialog.NewCustom(title, "Close", list, a.win)
	d.Resize(fyne.NewSize(formDialogWidth, searchDialogHeight))
	d.Show()
}

// searchResultRow renders one search match as a word-wrapped rich-text row: a
// dim date, a bold sender, and the body with occurrences of query highlighted.
func searchResultRow(m message, query string) fyne.CanvasObject {
	rt := widget.NewRichText()
	rt.Wrapping = fyne.TextWrapWord
	senderColor := theme.ColorNameForeground
	if m.own {
		senderColor = theme.ColorNamePrimary
	}
	segs := []widget.RichTextSegment{
		&widget.TextSegment{Text: m.ts + "  ", Style: widget.RichTextStyle{Inline: true, ColorName: theme.ColorNameDisabled}},
		&widget.TextSegment{Text: m.sender + "  ", Style: widget.RichTextStyle{Inline: true, TextStyle: fyne.TextStyle{Bold: true}, ColorName: senderColor}},
	}
	rt.Segments = append(segs, highlightSegments(m.body, query)...)
	rt.Refresh()
	return rt
}

// highlightSegments splits body into inline rich-text segments, bolding every
// case-insensitive occurrence of query so the match stands out, mirroring the
// server's LIKE substring match. Matching is on bytes, exact for ASCII queries.
func highlightSegments(body, query string) []widget.RichTextSegment {
	plain := func(s string) widget.RichTextSegment {
		return &widget.TextSegment{Text: s, Style: widget.RichTextStyle{Inline: true}}
	}
	if query == "" {
		return []widget.RichTextSegment{plain(body)}
	}
	var segs []widget.RichTextSegment
	lowerBody, lowerQuery := strings.ToLower(body), strings.ToLower(query)
	for {
		i := strings.Index(lowerBody, lowerQuery)
		if i < 0 {
			segs = append(segs, plain(body))
			break
		}
		if i > 0 {
			segs = append(segs, plain(body[:i]))
		}
		segs = append(segs, &widget.TextSegment{
			Text:  body[i : i+len(lowerQuery)],
			Style: widget.RichTextStyle{Inline: true, TextStyle: fyne.TextStyle{Bold: true}, ColorName: theme.ColorNamePrimary},
		})
		body = body[i+len(lowerQuery):]
		lowerBody = lowerBody[i+len(lowerQuery):]
	}
	return segs
}

// messageRow renders one scrollback entry as a word-wrapped rich-text row.
func messageRow(m message) fyne.CanvasObject {
	rt := widget.NewRichText()
	rt.Wrapping = fyne.TextWrapWord

	switch m.kind {
	case kindChat:
		var segs []widget.RichTextSegment
		if m.ts != "" {
			segs = append(segs, &widget.TextSegment{
				Text:  m.ts + "  ",
				Style: widget.RichTextStyle{Inline: true, ColorName: theme.ColorNameDisabled},
			})
		}
		senderColor := theme.ColorNameForeground
		if m.own {
			senderColor = theme.ColorNamePrimary
		}
		segs = append(segs,
			&widget.TextSegment{
				Text:  m.sender + "  ",
				Style: widget.RichTextStyle{Inline: true, TextStyle: fyne.TextStyle{Bold: true}, ColorName: senderColor},
			},
			&widget.TextSegment{Text: m.body, Style: widget.RichTextStyle{Inline: true}},
		)
		rt.Segments = segs
	case kindSystem:
		rt.Segments = []widget.RichTextSegment{&widget.TextSegment{
			Text:  m.body,
			Style: widget.RichTextStyle{Inline: true, ColorName: theme.ColorNameDisabled, TextStyle: fyne.TextStyle{Italic: true}},
		}}
	case kindOK:
		rt.Segments = []widget.RichTextSegment{&widget.TextSegment{
			Text:  m.body,
			Style: widget.RichTextStyle{Inline: true, ColorName: theme.ColorNameSuccess},
		}}
	case kindError:
		rt.Segments = []widget.RichTextSegment{&widget.TextSegment{
			Text:  m.body,
			Style: widget.RichTextStyle{Inline: true, ColorName: theme.ColorNameError},
		}}
	}
	rt.Refresh()
	return rt
}
