package chat

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Palette. Colours are 256-colour ANSI codes so the client renders the same on
// any reasonably modern terminal.
const (
	colAccent  = lipgloss.Color("63")  // brand blue/violet
	colSelect  = lipgloss.Color("212") // selection pink
	colDim     = lipgloss.Color("241")
	colDimmer  = lipgloss.Color("238")
	colErr     = lipgloss.Color("203")
	colOK      = lipgloss.Color("42")
	colUnread  = lipgloss.Color("220")
	colBarBG   = lipgloss.Color("236")
	colHeadBG  = lipgloss.Color("237")
	colHeadFG  = lipgloss.Color("254")
	colSelfMsg = lipgloss.Color("147") // the local user's own messages
)

// senderCol is the fixed width of the sender column in message and search rows.
const senderCol = 9

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	dimStyle   = lipgloss.NewStyle().Foreground(colDim)
	errStyle   = lipgloss.NewStyle().Foreground(colErr)
	okStyle    = lipgloss.NewStyle().Foreground(colOK)

	// Sidebar.
	sidebarStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.NormalBorder()).BorderRight(true).
			BorderForeground(colDimmer).Width(sidebarWidth).PaddingLeft(1)
	sectionStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Bold(true)
	sectionFocused  = lipgloss.NewStyle().Foreground(colSelect).Bold(true)
	selectBarStyle  = lipgloss.NewStyle().Foreground(colSelect).Bold(true)
	selectNameStyle = lipgloss.NewStyle().Foreground(colSelect).Bold(true)
	activeBarStyle  = lipgloss.NewStyle().Foreground(colAccent)
	activeStyle     = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	unreadStyle     = lipgloss.NewStyle().Bold(true).Foreground(colUnread)
	countStyle      = lipgloss.NewStyle().Foreground(colDimmer)
	onlineDot       = lipgloss.NewStyle().Foreground(colOK).Render("●")
	offlineDot      = dimStyle.Render("○")

	// Content pane.
	headerStyle   = lipgloss.NewStyle().Background(colHeadBG).Foreground(colHeadFG).Bold(true)
	headerHint    = lipgloss.NewStyle().Background(colHeadBG).Foreground(colDim)
	headerBadgeOK = lipgloss.NewStyle().Background(colHeadBG).Foreground(colOK)
	promptStyle   = lipgloss.NewStyle().Foreground(colAccent).Bold(true)

	// Scrollback message styling.
	systemMsgStyle = lipgloss.NewStyle().Foreground(colDim).Italic(true)
	selfMsgStyle   = lipgloss.NewStyle().Foreground(colSelfMsg).Bold(true)

	// searchHitStyle highlights the matched query text within a search result.
	searchHitStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(colUnread).Bold(true)
)

// senderPalette gives each participant a stable colour, hashed from their name,
// so messages are easy to scan by author.
var senderPalette = []lipgloss.Color{
	"75", "114", "139", "174", "180", "176", "115", "109", "209", "150", "146", "223",
}

func senderColor(name string) lipgloss.Color {
	var h uint32 = 2166136261
	for i := 0; i < len(name); i++ {
		h ^= uint32(name[i])
		h *= 16777619
	}
	return senderPalette[h%uint32(len(senderPalette))]
}

func (m *Model) View() string {
	if !m.loggedIn {
		return m.loginView()
	}
	if m.pw != nil {
		return m.passwordView()
	}
	if m.search != nil {
		return m.searchView()
	}
	return m.mainView()
}

// searchView draws the /search results overlay: a centred, bordered card with
// the query and match count, the scrollable results viewport, and a key hint.
// It replaces the chat view while open (like the login and /passwd screens) so
// the results read as a deliberate, dismissable view.
func (m *Model) searchView() string {
	matches := "matches"
	if m.search.count == 1 {
		matches = "match"
	}
	title := titleStyle.Render(fmt.Sprintf("Search: %q", m.search.query)) +
		countStyle.Render(fmt.Sprintf("  (%d %s)", m.search.count, matches))
	footer := dimStyle.Render("↑/↓ scroll · Esc close")
	body := lipgloss.JoinVertical(lipgloss.Left, title, "", m.search.vp.View(), "", footer)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).
		Padding(1, 2).Render(body)
	if m.width > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	}
	return box
}

// passwordView draws the /passwd modal: a centred, bordered card with the three
// masked fields, an error or hint line, and key reminders. It replaces the chat
// view while open (like the login screen) so it reads as a deliberate, modal
// action rather than something that fits in the message flow.
func (m *Model) passwordView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Change password"))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("Verify with your current password, then set a new one."))
	b.WriteString("\n\n")
	for i, in := range m.pw.inputs {
		caret := "  "
		if i == m.pw.focus {
			caret = promptStyle.Render("› ")
		}
		b.WriteString(caret)
		b.WriteString(in.View())
		b.WriteString("\n")
	}
	b.WriteString("\n")
	switch {
	case m.pw.busy:
		b.WriteString(dimStyle.Render("changing…"))
	case m.pw.err != "":
		b.WriteString(errStyle.Render("✗ " + m.pw.err))
	default:
		b.WriteString(dimStyle.Render("Tab to switch · Enter to save · Esc to cancel"))
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).
		Padding(1, 3).Render(b.String())
	if m.width > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	}
	return box
}

func (m *Model) loginView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("quorum"))
	b.WriteString(dimStyle.Render("  ·  secure chat"))
	b.WriteString("\n\n")
	b.WriteString(m.inputs[0].View())
	b.WriteString("\n")
	b.WriteString(m.inputs[1].View())
	b.WriteString("\n\n")
	switch {
	case m.loggingIn:
		b.WriteString(dimStyle.Render("logging in…"))
	case m.loginErr != "":
		b.WriteString(errStyle.Render("✗ " + m.loginErr))
	default:
		b.WriteString(dimStyle.Render("Tab to switch · Enter to log in"))
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).
		Padding(1, 3).Render(b.String())
	if m.width > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	}
	return box
}

func (m *Model) mainView() string {
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.sidebarView(), m.contentView())
	return lipgloss.JoinVertical(lipgloss.Left, body, m.statusBar())
}

// sidebarView draws two independently scrolled panels - channels on top, DMs
// below - separated by a blank line. Each panel emits exactly its allotted
// number of rows so the sidebar's height is fixed regardless of how many
// conversations exist, which keeps overflow from pushing the body off-screen.
func (m *Model) sidebarView() string {
	chListH, dmListH := m.listHeights()
	lines := []string{m.panelHeader("CHANNELS", focusChannels, m.chScroll, len(m.chOrder), chListH)}
	lines = append(lines, m.listRows(m.chOrder, m.chIdx, m.chScroll, chListH, m.focus == focusChannels, dimStyle.Render("  /join or /create"))...)
	lines = append(lines, "", m.panelHeader("DMS", focusDMs, m.dmScroll, len(m.dmOrder), dmListH))
	lines = append(lines, m.listRows(m.dmOrder, m.dmIdx, m.dmScroll, dmListH, m.focus == focusDMs, dimStyle.Render("  /dm <user>"))...)
	h := m.sidebarHeight()
	return sidebarStyle.Height(h).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

// panelHeader renders a panel's section title with a dim total count, highlighted
// when the panel holds focus, plus an arrow showing whether the list is scrolled
// or has more rows off-screen above (↑), below (↓), or both (↕).
func (m *Model) panelHeader(title string, panel focusArea, scroll, n, listH int) string {
	scroll = clampScroll(scroll, listH, n)
	ind := ""
	switch {
	case scroll > 0 && scroll+listH < n:
		ind = " ↕"
	case scroll > 0:
		ind = " ↑"
	case scroll+listH < n:
		ind = " ↓"
	}
	style := sectionStyle
	if m.focus == panel {
		style = sectionFocused
	}
	head := style.Render(title)
	if n > 0 {
		head += countStyle.Render(fmt.Sprintf(" %d", n))
	}
	return head + style.Render(ind)
}

// listRows renders a panel's visible window as exactly listH lines, padding with
// blanks when the list is short and showing emptyHint when it has no entries.
func (m *Model) listRows(order []string, sel, scroll, listH int, focused bool, emptyHint string) []string {
	rows := make([]string, 0, listH)
	if len(order) == 0 {
		if listH > 0 {
			rows = append(rows, emptyHint)
		}
	} else {
		scroll = clampScroll(scroll, listH, len(order))
		end := min(scroll+listH, len(order))
		for i := scroll; i < end; i++ {
			rows = append(rows, m.renderRow(order[i], focused && i == sel, order[i] == m.activeKey))
		}
	}
	for len(rows) < listH {
		rows = append(rows, "")
	}
	return rows
}

// renderRow renders a single sidebar entry: a selection/active marker bar, the
// conversation name, DM presence/encryption indicators, and an unread count. The
// marker keeps a fixed two-column prefix so sidebarHit's row math stays valid.
func (m *Model) renderRow(key string, selected, active bool) string {
	conv := m.convs[key]
	label := conv.name
	if maxLen := sidebarWidth - 6; len(label) > maxLen {
		label = label[:maxLen] + "…"
	}
	switch {
	case selected:
		label = selectBarStyle.Render("▎") + selectNameStyle.Render(" "+label)
	case active:
		label = activeBarStyle.Render("▎") + activeStyle.Render(" "+label)
	case !conv.isDM && !conv.joined:
		label = dimStyle.Render("  " + label)
	default:
		label = "  " + label
	}
	if conv.isDM {
		if conv.online {
			label += " " + onlineDot
		} else {
			label += " " + offlineDot
		}
		if conv.established {
			label += " 🔒"
		}
	}
	if conv.unread > 0 {
		label += unreadStyle.Render(fmt.Sprintf(" %d", conv.unread))
	}
	return label
}

// sidebarHit maps a click's Y coordinate to a conversation key and the panel it
// belongs to, mirroring sidebarView's layout: a CHANNELS header at row 0, the
// channels window, a blank line, a DMS header, then the DMs window. It returns
// ("", 0) for header rows, the blank separator, and rows past the last entry.
func (m *Model) sidebarHit(y int) (string, focusArea) {
	chListH, dmListH := m.listHeights()
	if y >= 1 && y <= chListH { // channel rows follow the CHANNELS header
		if idx := m.chScroll + (y - 1); idx < len(m.chOrder) {
			return m.chOrder[idx], focusChannels
		}
		return "", 0
	}
	dmFirst := chListH + 3 // CHANNELS header + channel window + blank + DMS header
	if y >= dmFirst && y < dmFirst+dmListH {
		if idx := m.dmScroll + (y - dmFirst); idx < len(m.dmOrder) {
			return m.dmOrder[idx], focusDMs
		}
	}
	return "", 0
}

func (m *Model) contentView() string {
	w := m.contentWidth()
	input := promptStyle.Render("›") + " " + m.input.View()
	// Only pad-left here; do NOT set Width. The header, viewport, and input are
	// already sized to w, and the viewport clips its own height. Setting Width on
	// this outer style makes lipgloss re-wrap the block at (w - leftPadding), so
	// every full-width line - the ones a long or contiguous message produces -
	// folds onto a second line. That inflates the content column past the
	// sidebar's height, pushing the whole view off-screen and desyncing the
	// sidebar's click hit-testing from what's drawn.
	return lipgloss.NewStyle().PaddingLeft(1).Render(
		lipgloss.JoinVertical(lipgloss.Left, m.headerBar(w), m.vp.View(), input))
}

// headerBar draws the title bar above the message viewport: the active
// conversation's name on the left and, for DMs, presence and an encryption badge
// right-aligned. It fills the full content width with a subtle background so it
// reads as a distinct bar without spending an extra row on a rule.
func (m *Model) headerBar(w int) string {
	conv := m.active()
	if conv == nil {
		return headerStyle.Width(w).Render(" quorum")
	}
	left := " " + conv.name
	right := ""
	if conv.isDM {
		// ASCII-only badges: glyphs like ● ○ · are East-Asian "ambiguous" width,
		// which lipgloss measures as 1 but many terminals render as 2. On a
		// full-width line that mismatch wraps the row and shoves the layout (and
		// the status bar) around, so colour carries the meaning instead.
		if conv.online {
			right = headerBadgeOK.Render("online")
		} else {
			right = headerHint.Render("offline")
		}
		if conv.established {
			right += headerBadgeOK.Render("  encrypted")
		} else {
			right += headerHint.Render("  not yet encrypted")
		}
		right += headerStyle.Render(" ")
	}
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
		right = ""
	}
	line := headerStyle.Render(left) + headerStyle.Render(strings.Repeat(" ", gap)) + right
	return headerStyle.Width(w).MaxWidth(w).Render(line)
}

// welcomeView fills the message area when no conversation is open yet.
func (m *Model) welcomeView() string {
	lines := []string{
		titleStyle.Render("Welcome to quorum"),
		"",
		dimStyle.Render("Choose a channel or DM from the sidebar to begin."),
		"",
		dimStyle.Render("Tab    focus the sidebar"),
		dimStyle.Render("↑ ↓    move    Enter open"),
		dimStyle.Render("/help  all commands"),
	}
	block := lipgloss.JoinVertical(lipgloss.Center, lines...)
	return lipgloss.Place(m.contentWidth(), m.contentHeight(), lipgloss.Center, lipgloss.Center, block)
}

// renderMessage turns one scrollback entry into display lines, wrapped to the
// content width. Chat lines get a dim timestamp, a colour-coded sender column,
// and a hanging indent so wrapped bodies line up under the first word.
func renderMessage(msg message, w int) string {
	switch msg.kind {
	case kindSystem:
		return systemMsgStyle.Width(w).Render(msg.body)
	case kindOK:
		return okStyle.Width(w).Render(msg.body)
	case kindError:
		return errStyle.Width(w).Render("⚠ " + msg.body)
	case kindHelp:
		return dimStyle.Width(w).Render(msg.body)
	}

	name := truncate(msg.sender, senderCol)
	nameStyle := lipgloss.NewStyle().Foreground(senderColor(msg.sender)).Bold(true)
	if msg.own {
		nameStyle = selfMsgStyle
	}
	gutter := dimStyle.Render(msg.ts) + " " + nameStyle.Render(pad(name, senderCol)) + "  "
	gutterW := len(msg.ts) + 1 + senderCol + 2
	bodyW := max(8, w-gutterW)

	bodyStyle := lipgloss.NewStyle().Width(bodyW)
	if msg.own {
		bodyStyle = bodyStyle.Foreground(colSelfMsg)
	}
	wrapped := strings.Split(bodyStyle.Render(msg.body), "\n")
	var b strings.Builder
	indent := strings.Repeat(" ", gutterW)
	for i, ln := range wrapped {
		if i == 0 {
			b.WriteString(gutter)
		} else {
			b.WriteByte('\n')
			b.WriteString(indent)
		}
		b.WriteString(ln)
	}
	return b.String()
}

// renderSearchResult formats one /search match: a dim date+time, a colour-coded
// sender column, and the body with the matched query text highlighted, wrapped
// to width w with a hanging indent like renderMessage's chat lines.
func renderSearchResult(msg message, w int, query string) string {
	name := truncate(msg.sender, senderCol)
	nameStyle := lipgloss.NewStyle().Foreground(senderColor(msg.sender)).Bold(true)
	if msg.own {
		nameStyle = selfMsgStyle
	}
	gutter := dimStyle.Render(msg.ts) + " " + nameStyle.Render(pad(name, senderCol)) + "  "
	gutterW := len(msg.ts) + 1 + senderCol + 2
	bodyW := max(8, w-gutterW)

	wrapped := strings.Split(lipgloss.NewStyle().Width(bodyW).Render(highlightLike(msg.body, query)), "\n")
	var b strings.Builder
	indent := strings.Repeat(" ", gutterW)
	for i, ln := range wrapped {
		if i == 0 {
			b.WriteString(gutter)
		} else {
			b.WriteByte('\n')
			b.WriteString(indent)
		}
		b.WriteString(ln)
	}
	return b.String()
}

// highlightLike wraps every case-insensitive occurrence of query in body with
// searchHitStyle, mirroring the server's LIKE substring match so the user sees
// exactly what matched. Matching is on bytes, which is exact for ASCII queries.
func highlightLike(body, query string) string {
	if query == "" {
		return body
	}
	lowerBody, lowerQuery := strings.ToLower(body), strings.ToLower(query)
	var b strings.Builder
	for {
		i := strings.Index(lowerBody, lowerQuery)
		if i < 0 {
			b.WriteString(body)
			break
		}
		b.WriteString(body[:i])
		b.WriteString(searchHitStyle.Render(body[i : i+len(lowerQuery)]))
		body = body[i+len(lowerQuery):]
		lowerBody = lowerBody[i+len(lowerQuery):]
	}
	return b.String()
}

// statusBar draws the bottom bar: a transient note on the left, the active DM's
// key plus the local user and connection state pinned to the right. Every
// segment carries the bar background explicitly - a foreground-only style would
// reset to the terminal's default (black) background between segments - and the
// bar uses ASCII only so an ambiguous-width glyph can never wrap it.
func (m *Model) statusBar() string {
	w := max(0, m.width)
	bg := lipgloss.NewStyle().Background(colBarBG)

	right := bg.Foreground(colHeadFG).Bold(true).Render(m.client.Username()) + bg.Render("  ")
	if m.connState == 0 { // ConnOnline
		right += bg.Foreground(colOK).Render("online")
	} else {
		right += bg.Foreground(colErr).Render(m.connState.String())
	}
	if conv := m.active(); conv != nil && conv.isDM && conv.fingerprint != "" {
		right = bg.Foreground(colDim).Render("key "+conv.fingerprint) + bg.Render("   ") + right
	}

	inner := w - 2 // one background-filled column of padding on each side
	note := m.statusNote
	if lipgloss.Width(note) > inner-lipgloss.Width(right)-1 {
		note = "" // too narrow to show the note without crowding the cluster
	}
	gap := max(1, inner-lipgloss.Width(note)-lipgloss.Width(right))
	return bg.Render(" ") + bg.Render(note) + bg.Render(strings.Repeat(" ", gap)) + right + bg.Render(" ")
}

// truncate shortens s to at most n runes, appending an ellipsis when it cuts.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// pad right-pads s with spaces to a visible width of n runes.
func pad(s string, n int) string {
	if d := n - len([]rune(s)); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}
