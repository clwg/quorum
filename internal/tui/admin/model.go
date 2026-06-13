// Package admin implements the server-management TUI: user and bot
// administration over the AdminService (role-gated server side).
package admin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	quorumv1 "github.com/clwg/quorum/gen/quorum/v1"
	"github.com/clwg/quorum/internal/client"
)

type tab int

const (
	tabUsers tab = iota
	tabBots
)

type mode int

const (
	modeList mode = iota
	modeLogin
	modeForm    // add user / reset password / create bot
	modeConfirm // delete confirmation
	modeToken   // show a bot token once
)

type formKind int

const (
	formAddUser formKind = iota
	formResetPassword
	formCreateBot
)

type loginResultMsg struct{ err error }
type usersMsg struct {
	users []*quorumv1.AdminUser
	err   error
}
type botsMsg struct {
	bots []*quorumv1.Bot
	err  error
}
type actionDoneMsg struct {
	note  string
	token string // set when a bot token must be displayed
	err   error
}

type Model struct {
	client *client.Client

	mode    mode
	tab     tab
	width   int
	height  int
	note    string
	noteErr bool

	// login
	inputs     []textinput.Model
	loginFocus int
	busy       bool

	// tables
	usersTable table.Model
	botsTable  table.Model
	users      []*quorumv1.AdminUser
	bots       []*quorumv1.Bot

	// form
	form       formKind
	formInputs []textinput.Model
	formFocus  int
	formTarget string // user ID for reset password

	// confirm / token display
	confirmTarget string
	confirmName   string
	shownToken    string
}

func New(c *client.Client) *Model {
	username := textinput.New()
	username.Placeholder = "admin username"
	username.Focus()
	password := textinput.New()
	password.Placeholder = "password"
	password.EchoMode = textinput.EchoPassword

	ut := table.New(table.WithColumns(userColumns(72)), table.WithFocused(true), table.WithHeight(12))
	ut.SetStyles(tableStyles())
	bt := table.New(table.WithColumns(botColumns(72)), table.WithFocused(true), table.WithHeight(12))
	bt.SetStyles(tableStyles())

	return &Model{
		client:     c,
		mode:       modeLogin,
		inputs:     []textinput.Model{username, password},
		usersTable: ut,
		botsTable:  bt,
	}
}

func (m *Model) Init() tea.Cmd { return textinput.Blink }

func rpcCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func (m *Model) refreshUsers() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := rpcCtx()
		defer cancel()
		resp, err := m.client.Admin().ListUsers(ctx, &quorumv1.AdminListUsersRequest{})
		if err != nil {
			return usersMsg{err: err}
		}
		return usersMsg{users: resp.GetUsers()}
	}
}

func (m *Model) refreshBots() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := rpcCtx()
		defer cancel()
		resp, err := m.client.Admin().ListBots(ctx, &quorumv1.ListBotsRequest{})
		if err != nil {
			return botsMsg{err: err}
		}
		return botsMsg{bots: resp.GetBots()}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		h := max(6, m.height-7)
		m.usersTable.SetHeight(h)
		m.botsTable.SetHeight(h)
		tw := max(48, m.width-2)
		m.usersTable.SetColumns(userColumns(tw))
		m.botsTable.SetColumns(botColumns(tw))
		return m, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
	case usersMsg:
		if msg.err != nil {
			return m.fail("users", msg.err)
		}
		// Bots are accounts too, but they belong on the Bots tab only; keep
		// m.users aligned with the rendered rows so the cursor maps correctly.
		m.users = m.users[:0]
		rows := make([]table.Row, 0, len(msg.users))
		for _, u := range msg.users {
			if u.GetRole() == "bot" {
				continue
			}
			m.users = append(m.users, u)
			status := "active"
			if u.GetDisabled() {
				status = "disabled"
			}
			rows = append(rows, table.Row{u.GetUsername(), u.GetRole(), status, u.GetCreatedAt().AsTime().Local().Format("2006-01-02 15:04")})
		}
		m.usersTable.SetRows(rows)
		return m, nil
	case botsMsg:
		if msg.err != nil {
			return m.fail("bots", msg.err)
		}
		m.bots = msg.bots
		rows := make([]table.Row, 0, len(m.bots))
		for _, b := range m.bots {
			rows = append(rows, table.Row{b.GetUsername(), b.GetOwnerName(), b.GetCreatedAt().AsTime().Local().Format("2006-01-02 15:04")})
		}
		m.botsTable.SetRows(rows)
		return m, nil
	case actionDoneMsg:
		m.busy = false
		if msg.err != nil {
			return m.fail("", msg.err)
		}
		m.note, m.noteErr = msg.note, false
		if msg.token != "" {
			m.shownToken = msg.token
			m.mode = modeToken
		} else {
			m.mode = modeList
		}
		return m, tea.Batch(m.refreshUsers(), m.refreshBots())
	case loginResultMsg:
		m.busy = false
		if msg.err != nil {
			m.note, m.noteErr = errText(msg.err), true
			return m, nil
		}
		m.mode = modeList
		m.note = "" // header bar already shows the signed-in admin
		return m, tea.Batch(m.refreshUsers(), m.refreshBots())
	}

	switch m.mode {
	case modeLogin:
		return m.updateLogin(msg)
	case modeForm:
		return m.updateForm(msg)
	case modeConfirm:
		return m.updateConfirm(msg)
	case modeToken:
		if k, ok := msg.(tea.KeyMsg); ok && (k.Type == tea.KeyEnter || k.Type == tea.KeyEsc) {
			m.shownToken = ""
			m.mode = modeList
		}
		return m, nil
	default:
		return m.updateList(msg)
	}
}

func (m *Model) fail(ctx string, err error) (tea.Model, tea.Cmd) {
	if ctx != "" {
		m.note = ctx + ": " + errText(err)
	} else {
		m.note = errText(err)
	}
	m.noteErr = true
	m.busy = false
	if m.mode != modeLogin {
		m.mode = modeList
	}
	return m, nil
}

func (m *Model) updateLogin(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.Type {
		case tea.KeyTab, tea.KeyShiftTab, tea.KeyUp, tea.KeyDown:
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
			if m.busy {
				return m, nil
			}
			username, password := strings.TrimSpace(m.inputs[0].Value()), m.inputs[1].Value()
			if username == "" || password == "" {
				m.note, m.noteErr = "username and password required", true
				return m, nil
			}
			m.busy = true
			m.note = ""
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				return loginResultMsg{err: m.client.Login(ctx, username, password)}
			}
		}
	}
	var cmds []tea.Cmd
	for i := range m.inputs {
		var cmd tea.Cmd
		m.inputs[i], cmd = m.inputs[i].Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m *Model) updateList(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "1":
		m.tab = tabUsers
		return m, nil
	case "2":
		m.tab = tabBots
		return m, nil
	case "q":
		return m, tea.Quit
	case "R":
		return m, tea.Batch(m.refreshUsers(), m.refreshBots())
	}

	if m.tab == tabUsers {
		switch key.String() {
		case "a":
			return m.openForm(formAddUser, "username", "password", "role (user|admin, default user)")
		case "d":
			if u := m.selectedUser(); u != nil {
				return m, m.toggleDisabled(u)
			}
		case "x":
			if u := m.selectedUser(); u != nil {
				m.confirmTarget, m.confirmName = u.GetId(), u.GetUsername()
				m.mode = modeConfirm
			}
			return m, nil
		case "r":
			if u := m.selectedUser(); u != nil {
				m.formTarget = u.GetId()
				return m.openForm(formResetPassword, "new password for "+u.GetUsername())
			}
		}
		var cmd tea.Cmd
		m.usersTable, cmd = m.usersTable.Update(msg)
		return m, cmd
	}

	switch key.String() {
	case "a":
		return m.openForm(formCreateBot, "bot username")
	case "t":
		if b := m.selectedBot(); b != nil {
			userID := b.GetUserId()
			return m, func() tea.Msg {
				ctx, cancel := rpcCtx()
				defer cancel()
				resp, err := m.client.Admin().RevokeBotToken(ctx, &quorumv1.RevokeBotTokenRequest{UserId: userID})
				if err != nil {
					return actionDoneMsg{err: err}
				}
				return actionDoneMsg{note: "token rotated", token: resp.GetNewToken()}
			}
		}
	case "x":
		if b := m.selectedBot(); b != nil {
			m.confirmTarget, m.confirmName = b.GetUserId(), b.GetUsername()+" (bot)"
			m.mode = modeConfirm
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.botsTable, cmd = m.botsTable.Update(msg)
	return m, cmd
}

func (m *Model) selectedUser() *quorumv1.AdminUser {
	i := m.usersTable.Cursor()
	if i < 0 || i >= len(m.users) {
		return nil
	}
	return m.users[i]
}

func (m *Model) selectedBot() *quorumv1.Bot {
	i := m.botsTable.Cursor()
	if i < 0 || i >= len(m.bots) {
		return nil
	}
	return m.bots[i]
}

func (m *Model) toggleDisabled(u *quorumv1.AdminUser) tea.Cmd {
	id, disable := u.GetId(), !u.GetDisabled()
	return func() tea.Msg {
		ctx, cancel := rpcCtx()
		defer cancel()
		_, err := m.client.Admin().SetUserDisabled(ctx, &quorumv1.SetUserDisabledRequest{UserId: id, Disabled: disable})
		if err != nil {
			return actionDoneMsg{err: err}
		}
		verb := "enabled"
		if disable {
			verb = "disabled (sessions revoked, stream kicked)"
		}
		return actionDoneMsg{note: u.GetUsername() + " " + verb}
	}
}

func (m *Model) openForm(kind formKind, placeholders ...string) (tea.Model, tea.Cmd) {
	m.form = kind
	m.formInputs = nil
	for i, ph := range placeholders {
		in := textinput.New()
		in.Placeholder = ph
		if strings.Contains(ph, "password") {
			in.EchoMode = textinput.EchoPassword
		}
		if i == 0 {
			in.Focus()
		}
		m.formInputs = append(m.formInputs, in)
	}
	m.formFocus = 0
	m.mode = modeForm
	return m, textinput.Blink
}

func (m *Model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.Type {
		case tea.KeyEsc:
			m.mode = modeList
			return m, nil
		case tea.KeyTab, tea.KeyShiftTab, tea.KeyDown, tea.KeyUp:
			m.formFocus = (m.formFocus + 1) % len(m.formInputs)
			for i := range m.formInputs {
				if i == m.formFocus {
					m.formInputs[i].Focus()
				} else {
					m.formInputs[i].Blur()
				}
			}
			return m, nil
		case tea.KeyEnter:
			return m.submitForm()
		}
	}
	var cmds []tea.Cmd
	for i := range m.formInputs {
		var cmd tea.Cmd
		m.formInputs[i], cmd = m.formInputs[i].Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m *Model) submitForm() (tea.Model, tea.Cmd) {
	vals := make([]string, len(m.formInputs))
	for i := range m.formInputs {
		vals[i] = strings.TrimSpace(m.formInputs[i].Value())
	}
	switch m.form {
	case formAddUser:
		username, password, role := vals[0], vals[1], vals[2]
		if role == "" {
			role = "user"
		}
		return m, func() tea.Msg {
			ctx, cancel := rpcCtx()
			defer cancel()
			_, err := m.client.Admin().CreateUser(ctx, &quorumv1.CreateUserRequest{Username: username, Password: password, Role: role})
			if err != nil {
				return actionDoneMsg{err: err}
			}
			return actionDoneMsg{note: "created user " + username}
		}
	case formResetPassword:
		target, password := m.formTarget, vals[0]
		return m, func() tea.Msg {
			ctx, cancel := rpcCtx()
			defer cancel()
			_, err := m.client.Admin().ResetPassword(ctx, &quorumv1.ResetPasswordRequest{UserId: target, NewPassword: password})
			if err != nil {
				return actionDoneMsg{err: err}
			}
			return actionDoneMsg{note: "password reset; their sessions were revoked"}
		}
	case formCreateBot:
		username := vals[0]
		return m, func() tea.Msg {
			ctx, cancel := rpcCtx()
			defer cancel()
			resp, err := m.client.Admin().CreateBot(ctx, &quorumv1.CreateBotRequest{Username: username})
			if err != nil {
				return actionDoneMsg{err: err}
			}
			return actionDoneMsg{note: "created bot " + username, token: resp.GetToken()}
		}
	}
	return m, nil
}

func (m *Model) updateConfirm(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "y", "Y":
		target := m.confirmTarget
		m.mode = modeList
		return m, func() tea.Msg {
			ctx, cancel := rpcCtx()
			defer cancel()
			_, err := m.client.Admin().DeleteUser(ctx, &quorumv1.DeleteUserRequest{UserId: target})
			if err != nil {
				return actionDoneMsg{err: err}
			}
			return actionDoneMsg{note: "deleted"}
		}
	default:
		m.mode = modeList
		return m, nil
	}
}

func errText(err error) string {
	s := err.Error()
	if _, after, found := strings.Cut(s, "desc = "); found {
		return after
	}
	return s
}

// Palette. 256-colour ANSI codes, kept in step with the chat client so both
// TUIs read as one product.
const (
	colAccent = lipgloss.Color("63")  // brand blue/violet
	colSelect = lipgloss.Color("212") // selection pink
	colDim    = lipgloss.Color("241")
	colDimmer = lipgloss.Color("238")
	colErr    = lipgloss.Color("203")
	colOK     = lipgloss.Color("42")
	colWarn   = lipgloss.Color("220")
	colBarBG  = lipgloss.Color("236")
	colHeadBG = lipgloss.Color("237")
	colHeadFG = lipgloss.Color("254")
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	dimStyle    = lipgloss.NewStyle().Foreground(colDim)
	errStyle    = lipgloss.NewStyle().Foreground(colErr)
	okStyle     = lipgloss.NewStyle().Foreground(colOK)
	promptStyle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)

	activeTab   = lipgloss.NewStyle().Bold(true).Foreground(colHeadFG).Background(colAccent).Padding(0, 2)
	inactiveTab = lipgloss.NewStyle().Foreground(colDim).Padding(0, 2)

	cardStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).Padding(1, 3)
	confirmCard  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colErr).Padding(1, 3)
	tokenCard    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colWarn).Padding(1, 3)
	tokenValue   = lipgloss.NewStyle().Bold(true).Foreground(colWarn)
	contentStyle = lipgloss.NewStyle().Padding(1, 0, 0, 1)
)

// tableStyles gives both tables a dim, underlined header and a highlighted
// selected row that reads as a cursor bar.
func tableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = s.Header.
		Bold(true).Foreground(colDim).
		BorderStyle(lipgloss.NormalBorder()).BorderBottom(true).BorderForeground(colDimmer)
	s.Selected = lipgloss.NewStyle().Bold(true).Foreground(colSelect).Background(colBarBG)
	return s
}

// userColumns / botColumns lay the tables out to a target total width w
// (including the per-cell padding), letting the name columns absorb the slack
// so wide terminals don't leave a cramped 20-column username.
func userColumns(w int) []table.Column {
	const fixed = 6 + 9 + 16 + 2*4 // role + status + created + cell padding
	name := max(16, w-fixed)
	return []table.Column{
		{Title: "Username", Width: name},
		{Title: "Role", Width: 6},
		{Title: "Status", Width: 9},
		{Title: "Created", Width: 16},
	}
}

func botColumns(w int) []table.Column {
	const fixed = 16 + 2*3 // created + cell padding
	rest := max(28, w-fixed)
	bot := rest / 2
	return []table.Column{
		{Title: "Bot", Width: bot},
		{Title: "Owner", Width: rest - bot},
		{Title: "Created", Width: 16},
	}
}

func (m *Model) View() string {
	switch m.mode {
	case modeLogin:
		return m.modal(m.loginCard())
	case modeForm:
		return m.modal(m.formCard())
	case modeConfirm:
		return m.modal(confirmCard.Render(lipgloss.JoinVertical(lipgloss.Left,
			errStyle.Render("Delete "+m.confirmName+"?"),
			"",
			dimStyle.Render("This permanently removes the account and cannot be undone."),
			"",
			dimStyle.Render("y confirm · any other key cancel"))))
	case modeToken:
		return m.modal(tokenCard.Render(lipgloss.JoinVertical(lipgloss.Left,
			titleStyle.Render("Bot token"),
			dimStyle.Render("Copy it now — it is not stored and cannot be shown again."),
			"",
			tokenValue.Render(m.shownToken),
			"",
			dimStyle.Render("Enter to dismiss"))))
	}
	return m.listView()
}

// modal centres a card over the full screen, matching the chat client's login
// and password dialogs.
func (m *Model) modal(card string) string {
	if m.width > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, card)
	}
	return card
}

func (m *Model) loginCard() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("quorum admin"))
	b.WriteString(dimStyle.Render("  ·  server management"))
	b.WriteString("\n\n")
	for i, in := range m.inputs {
		b.WriteString(caret(i == m.loginFocus))
		b.WriteString(in.View())
		b.WriteByte('\n')
	}
	b.WriteString("\n")
	switch {
	case m.busy:
		b.WriteString(dimStyle.Render("authenticating…"))
	case m.note != "":
		b.WriteString(m.noteStyled())
	default:
		b.WriteString(dimStyle.Render("Tab to switch · Enter to sign in · Ctrl+C to quit"))
	}
	return cardStyle.Render(b.String())
}

func (m *Model) formCard() string {
	title, subtitle := m.formText()
	var b strings.Builder
	b.WriteString(titleStyle.Render(title))
	if subtitle != "" {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(subtitle))
	}
	b.WriteString("\n\n")
	for i, in := range m.formInputs {
		b.WriteString(caret(i == m.formFocus))
		b.WriteString(in.View())
		b.WriteByte('\n')
	}
	b.WriteString("\n")
	if m.note != "" && m.noteErr {
		b.WriteString(errStyle.Render("✗ " + m.note))
		b.WriteByte('\n')
	}
	b.WriteString(dimStyle.Render("Enter to submit · Esc to cancel"))
	return cardStyle.Render(b.String())
}

func (m *Model) formText() (title, subtitle string) {
	switch m.form {
	case formAddUser:
		return "Add user", "Create an account, then share the password to sign in."
	case formResetPassword:
		return "Reset password", "Set a new password. Their active sessions are revoked."
	case formCreateBot:
		return "Create bot", "Register a bot account. Its token is shown once."
	}
	return "quorum admin", ""
}

func (m *Model) listView() string {
	usersTab := m.tabStyle(tabUsers).Render(fmt.Sprintf("1  Users  %d", len(m.users)))
	botsTab := m.tabStyle(tabBots).Render(fmt.Sprintf("2  Bots  %d", len(m.bots)))
	tabs := lipgloss.JoinHorizontal(lipgloss.Top, usersTab, " ", botsTab)

	var body string
	var help [][2]string
	if m.tab == tabUsers {
		body = m.usersTable.View()
		help = [][2]string{{"1/2", "switch"}, {"j/k", "move"}, {"a", "add"}, {"d", "disable"}, {"r", "reset pw"}, {"x", "delete"}, {"R", "refresh"}, {"q", "quit"}}
	} else {
		body = m.botsTable.View()
		help = [][2]string{{"1/2", "switch"}, {"j/k", "move"}, {"a", "new bot"}, {"t", "rotate token"}, {"x", "delete"}, {"R", "refresh"}, {"q", "quit"}}
	}

	// Reserve the note row even when empty so the table height — and thus the
	// footer's position — stays fixed as transient notes come and go.
	note := ""
	if m.note != "" {
		note = m.noteStyled()
	}
	content := contentStyle.Render(lipgloss.JoinVertical(lipgloss.Left, tabs, "", body, "", note))

	// Pin the footer to the bottom: fill whatever vertical space the header,
	// content and footer don't already occupy.
	header, footer := m.headerBar(), m.footerBar(help)
	parts := []string{header, content}
	for fill := m.height - lipgloss.Height(header) - lipgloss.Height(content) - lipgloss.Height(footer); fill > 0; fill-- {
		parts = append(parts, "")
	}
	parts = append(parts, footer)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// headerBar draws the full-width title bar: product name on the left, the
// signed-in admin pinned right, on a subtle background.
func (m *Model) headerBar() string {
	w := max(0, m.width)
	bg := lipgloss.NewStyle().Background(colHeadBG)
	left := bg.Foreground(colHeadFG).Bold(true).Render(" quorum admin")
	right := bg.Foreground(colDim).Render("signed in ") + bg.Foreground(colHeadFG).Bold(true).Render(m.client.Username()) + bg.Render(" ")
	gap := max(1, w-lipgloss.Width(left)-lipgloss.Width(right))
	return left + bg.Render(strings.Repeat(" ", gap)) + right
}

// footerBar draws the full-width key-hint bar from key/description pairs.
func (m *Model) footerBar(pairs [][2]string) string {
	w := max(0, m.width)
	bg := lipgloss.NewStyle().Background(colBarBG)
	keyS := bg.Foreground(colHeadFG).Bold(true)
	descS := bg.Foreground(colDim)
	var b strings.Builder
	b.WriteString(bg.Render(" "))
	for i, p := range pairs {
		if i > 0 {
			b.WriteString(descS.Render("  "))
		}
		b.WriteString(keyS.Render(p[0]))
		b.WriteString(descS.Render(" " + p[1]))
	}
	line := b.String()
	gap := max(1, w-lipgloss.Width(line))
	return line + bg.Render(strings.Repeat(" ", gap))
}

// caret returns the focused-field marker used in the modal cards.
func caret(focused bool) string {
	if focused {
		return promptStyle.Render("› ")
	}
	return "  "
}

func (m *Model) tabStyle(t tab) lipgloss.Style {
	if m.tab == t {
		return activeTab
	}
	return inactiveTab
}

func (m *Model) noteStyled() string {
	if m.noteErr {
		return errStyle.Render("✗ " + m.note)
	}
	return okStyle.Render("✓ " + m.note)
}
