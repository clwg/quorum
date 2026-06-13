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

	quorumv1 "github.com/layer8/quorum/gen/quorum/v1"
	"github.com/layer8/quorum/internal/client"
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

	mode     mode
	tab      tab
	width    int
	height   int
	note     string
	noteErr  bool

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

	ut := table.New(table.WithColumns([]table.Column{
		{Title: "Username", Width: 20},
		{Title: "Role", Width: 8},
		{Title: "Disabled", Width: 9},
		{Title: "Created", Width: 16},
	}), table.WithFocused(true), table.WithHeight(12))
	bt := table.New(table.WithColumns([]table.Column{
		{Title: "Bot", Width: 20},
		{Title: "Owner", Width: 20},
		{Title: "Created", Width: 16},
	}), table.WithFocused(true), table.WithHeight(12))

	return &Model{
		client: c,
		mode:   modeLogin,
		inputs: []textinput.Model{username, password},
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
		h := max(6, m.height-10)
		m.usersTable.SetHeight(h)
		m.botsTable.SetHeight(h)
		return m, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
	case usersMsg:
		if msg.err != nil {
			return m.fail("users", msg.err)
		}
		m.users = msg.users
		rows := make([]table.Row, 0, len(m.users))
		for _, u := range m.users {
			dis := ""
			if u.GetDisabled() {
				dis = "yes"
			}
			rows = append(rows, table.Row{u.GetUsername(), u.GetRole(), dis, u.GetCreatedAt().AsTime().Local().Format("2006-01-02 15:04")})
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
		m.note = "logged in as " + m.client.Username()
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

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	activeTab   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")).Underline(true)
	inactiveTab = dimStyle
	tokenStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220")).Border(lipgloss.RoundedBorder()).Padding(1, 2)
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2)
)

func (m *Model) View() string {
	switch m.mode {
	case modeLogin:
		var b strings.Builder
		b.WriteString(titleStyle.Render("quorum admin") + "\n\n")
		b.WriteString("  " + m.inputs[0].View() + "\n")
		b.WriteString("  " + m.inputs[1].View() + "\n\n")
		if m.busy {
			b.WriteString(dimStyle.Render("  logging in…") + "\n")
		}
		if m.note != "" {
			b.WriteString(m.noteStyled() + "\n")
		}
		b.WriteString(dimStyle.Render("\n  Tab to switch · Enter to log in · Ctrl+C to quit"))
		return lipgloss.NewStyle().Padding(1, 2).Render(b.String())
	case modeForm:
		var b strings.Builder
		b.WriteString(titleStyle.Render("quorum admin") + "\n\n")
		for _, in := range m.formInputs {
			b.WriteString("  " + in.View() + "\n")
		}
		b.WriteString(dimStyle.Render("\n  Enter to submit · Esc to cancel"))
		return boxStyle.Render(b.String())
	case modeConfirm:
		return boxStyle.Render(fmt.Sprintf("Delete %s? This cannot be undone.\n\n%s",
			titleStyle.Render(m.confirmName), dimStyle.Render("y to confirm · any other key to cancel")))
	case modeToken:
		return tokenStyle.Render(fmt.Sprintf("Bot token (copy it now — it is not stored):\n\n  %s\n\nEnter to dismiss", m.shownToken))
	}

	tabs := lipgloss.JoinHorizontal(lipgloss.Top,
		m.tabStyle(tabUsers).Render("[1] Users"), "  ", m.tabStyle(tabBots).Render("[2] Bots"))
	var body, help string
	if m.tab == tabUsers {
		body = m.usersTable.View()
		help = "a add · d toggle disabled · r reset password · x delete · R refresh · q quit"
	} else {
		body = m.botsTable.View()
		help = "a create bot · t rotate token · x delete · R refresh · q quit"
	}
	out := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Render("quorum admin")+dimStyle.Render("  "+m.client.Username()),
		tabs, body, dimStyle.Render(help))
	if m.note != "" {
		out = lipgloss.JoinVertical(lipgloss.Left, out, m.noteStyled())
	}
	return lipgloss.NewStyle().Padding(1, 2).Render(out)
}

func (m *Model) tabStyle(t tab) lipgloss.Style {
	if m.tab == t {
		return activeTab
	}
	return inactiveTab
}

func (m *Model) noteStyled() string {
	if m.noteErr {
		return errStyle.Render(m.note)
	}
	return okStyle.Render(m.note)
}
