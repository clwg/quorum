package gui

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// loginFormWidth is the fixed width of the centred connection form.
const loginFormWidth = 420

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

// dmRowSegments renders a DM's sidebar row: a presence bullet — green and
// filled when the peer is online, hollow otherwise — the name, and an unread
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
	a.msgScroll.ScrollToBottom()
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
	dialog.ShowForm("Create channel", "Create", "Cancel",
		[]*widget.FormItem{widget.NewFormItem("Channel", entry)},
		func(ok bool) {
			if !ok {
				return
			}
			if name := strings.TrimSpace(entry.Text); name != "" {
				a.createChannel(name)
			}
		}, a.win)
}

// showHelp lists the client's slash commands in a dialog.
func (a *App) showHelp() {
	help := strings.Join([]string{
		"/create <name>   create a channel and open it",
		"/join <name>     join an existing channel",
		"/leave           leave the current channel",
		"/dm <user>       open an end-to-end-encrypted direct message",
		"/commands        list commands offered by bots",
		"/help            show this help",
		"/quit            exit quorum",
		"",
		"Click a channel or user in the sidebar to open it.",
		"The + above CHANNELS creates a new channel.",
	}, "\n")
	dialog.ShowInformation("Quorum commands", help, a.win)
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
