package gui

import (
	"strings"
	"time"

	quorumv1 "github.com/clwg/quorum/gen/quorum/v1"
)

// conversation is a single channel or DM thread in the sidebar, mirroring the
// model the TUI keeps but rendered with Fyne widgets. All fields are touched
// only on the UI goroutine.
type conversation struct {
	key    string // "ch:<id>" or "dm:<peerID>"
	id     string // channel ID or peer user ID
	name   string // "#general" or "@alice"
	isDM   bool
	joined bool // channels: membership
	online bool // DMs: peer presence
	unread int
	msgs   []message

	historyLoaded bool

	// Channel history pagination (scroll up to load older messages).
	oldestID     int64 // server id of the oldest loaded message; the next page's cursor
	hasMore      bool  // older messages may still exist on the server
	loadingOlder bool  // an older-history fetch is in flight

	// DM E2EE session state
	established bool
	fingerprint string
}

// msgKind selects how a scrollback entry is rendered.
type msgKind uint8

const (
	kindChat   msgKind = iota // a person's message: timestamp, sender, body
	kindSystem                // join/leave/notice, dimmed
	kindOK                    // a positive notice (e.g. session established)
	kindError                 // a warning/error
)

// message is one entry in a conversation's scrollback. Entries are stored
// structured rather than pre-formatted so the view can style senders, dim
// timestamps, and re-wrap on resize.
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
func sysLine(body string) message { return message{body: body, kind: kindSystem} }
func okLine(body string) message  { return message{body: body, kind: kindOK} }
func errLine(body string) message { return message{body: body, kind: kindError} }

func chKey(id string) string { return "ch:" + id }
func dmKey(id string) string { return "dm:" + id }

func fmtTime(cm *quorumv1.ChannelMessage) string {
	if cm.GetSentAt() == nil {
		return time.Now().Format("15:04")
	}
	return cm.GetSentAt().AsTime().Local().Format("15:04")
}

// fmtSearchDate formats a search match with its date, since matches can be days
// or weeks old where a bare clock time would be ambiguous.
func fmtSearchDate(cm *quorumv1.ChannelMessage) string {
	if cm.GetSentAt() == nil {
		return time.Now().Format("Jan 02 15:04")
	}
	return cm.GetSentAt().AsTime().Local().Format("Jan 02 15:04")
}

// grpcErrText strips the gRPC "desc = " prefix for display.
func grpcErrText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if _, after, found := strings.Cut(s, "desc = "); found {
		return after
	}
	return s
}
