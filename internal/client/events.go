package client

import quorumv1 "github.com/layer8/quorum/gen/quorum/v1"

// Event is anything delivered to the UI through Run's callback.
type Event any

type ConnState int

const (
	ConnOnline ConnState = iota
	ConnOffline
	ConnReconnecting
)

func (s ConnState) String() string {
	switch s {
	case ConnOnline:
		return "online"
	case ConnOffline:
		return "offline"
	case ConnReconnecting:
		return "reconnecting"
	}
	return "unknown"
}

type ConnStateEvent struct {
	State ConnState
	Err   error
}

// ResyncEvent fires after (re)subscribing: the UI should refetch channel
// lists and recent history.
type ResyncEvent struct{}

type ChannelMessageEvent struct{ Msg *quorumv1.ChannelMessage }
type PresenceEvent struct{ Presence *quorumv1.PresenceEvent }
type ChannelEventEvent struct{ Event *quorumv1.ChannelEvent }
type SystemEvent struct{ Notice *quorumv1.SystemNotice }

// DirectMessageEvent is a decrypted 1:1 message (or the local echo of one
// we sent - the server does not echo DMs).
type DirectMessageEvent struct {
	PeerID   string
	PeerName string
	Text     string
	Outgoing bool
}

// DMSessionEvent reports E2EE session lifecycle for a peer.
type DMSessionEvent struct {
	PeerID      string
	PeerName    string
	Established bool
	Fingerprint string // peer identity fingerprint
	Err         error  // set on handshake failure / TOFU mismatch
}

// ErrorEvent carries a non-fatal background error for display.
type ErrorEvent struct {
	Context string
	Err     error
}
