// Package client is the shared client core used by the chat TUI, the
// admin TUI, and the bot SDK: TLS dialing with server fingerprinting,
// login, the Subscribe event pump with reconnect, and the E2EE session
// manager for direct messages.
package client

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	quorumv1 "github.com/layer8/quorum/gen/quorum/v1"
	"github.com/layer8/quorum/internal/e2ee"
)

type Config struct {
	Addr    string
	CAFile  string // dev CA; empty = system roots
	DataDir string // default ~/.config/quorum
}

type Client struct {
	cfg      Config
	serverID string // hex SHA-256 of the server TLS public key (SPKI)
	conn     *grpc.ClientConn
	authc    quorumv1.AuthServiceClient
	chatc    quorumv1.ChatServiceClient
	adminc   quorumv1.AdminServiceClient

	mu       sync.Mutex
	token    string
	password string // kept in memory for automatic re-login on reconnect
	userID   string
	username string
	identity *ecdh.PrivateKey
	pins     *pinStore

	dm *dmManager

	onEvent func(Event)
}

// Dial connects to the server, verifying TLS against the configured CA,
// and records the server's public-key fingerprint, which scopes all local
// state (identity keys, TOFU pins) to this server.
func Dial(cfg Config) (*Client, error) {
	if cfg.DataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		cfg.DataDir = home + "/.config/quorum"
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pemBytes, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, errors.New("no certificates in CA file")
		}
		tlsCfg.RootCAs = pool
	}

	serverID, err := fetchServerID(cfg.Addr, tlsCfg.Clone())
	if err != nil {
		return nil, fmt.Errorf("server fingerprint: %w", err)
	}

	c := &Client{cfg: cfg, serverID: serverID}
	c.dm = newDMManager(c)

	conn, err := grpc.NewClient(cfg.Addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithUnaryInterceptor(c.unaryAuth),
		grpc.WithStreamInterceptor(c.streamAuth),
	)
	if err != nil {
		return nil, err
	}
	c.conn = conn
	c.authc = quorumv1.NewAuthServiceClient(conn)
	c.chatc = quorumv1.NewChatServiceClient(conn)
	c.adminc = quorumv1.NewAdminServiceClient(conn)
	return c, nil
}

// Admin exposes the AdminService client (RPCs succeed only for admins).
func (c *Client) Admin() quorumv1.AdminServiceClient { return c.adminc }

// fetchServerID does a throwaway verified TLS handshake to capture the
// server certificate's SubjectPublicKeyInfo fingerprint.
func fetchServerID(addr string, tlsCfg *tls.Config) (string, error) {
	d := &tls.Dialer{Config: tlsCfg}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("no server certificate")
	}
	sum := sha256.Sum256(certs[0].RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Client) getToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

func (c *Client) unaryAuth(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if tok := c.getToken(); tok != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

func (c *Client) streamAuth(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	if tok := c.getToken(); tok != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
	}
	return streamer(ctx, desc, cc, method, opts...)
}

// SetToken installs a pre-issued bearer token (bot tokens) instead of
// password login.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
}

// WhoAmI resolves and caches the caller's identity from the current
// token. Token-authenticated clients (bots) call this in place of Login.
func (c *Client) WhoAmI(ctx context.Context) (userID, username, role string, err error) {
	resp, err := c.authc.WhoAmI(ctx, &quorumv1.WhoAmIRequest{})
	if err != nil {
		return "", "", "", err
	}
	c.mu.Lock()
	c.userID = resp.GetUserId()
	c.username = resp.GetUsername()
	c.mu.Unlock()
	return resp.GetUserId(), resp.GetUsername(), resp.GetRole(), nil
}

// Login authenticates, loads (or creates) this user's identity key for
// this server, and publishes the public half to the directory.
func (c *Client) Login(ctx context.Context, username, password string) error {
	resp, err := c.authc.Login(ctx, &quorumv1.LoginRequest{Username: username, Password: password})
	if err != nil {
		return err
	}
	identity, err := loadOrCreateIdentity(c.cfg.DataDir, c.serverID, resp.GetUsername())
	if err != nil {
		return fmt.Errorf("identity key: %w", err)
	}
	pins, err := openPinStore(c.cfg.DataDir, c.serverID, resp.GetUsername())
	if err != nil {
		return fmt.Errorf("pin store: %w", err)
	}

	c.mu.Lock()
	c.token = resp.GetToken()
	c.password = password
	c.userID = resp.GetUserId()
	c.username = resp.GetUsername()
	c.identity = identity
	c.pins = pins
	c.mu.Unlock()

	if _, err := c.authc.PublishIdentityKey(ctx, &quorumv1.PublishIdentityKeyRequest{
		X25519PublicKey: identity.PublicKey().Bytes(),
	}); err != nil {
		return fmt.Errorf("publish identity key: %w", err)
	}
	return nil
}

// ChangePassword replaces the logged-in user's password after the server
// verifies the current one. On success it updates the in-memory credential
// used for automatic re-login on reconnect, so the live session keeps working
// with the new password.
func (c *Client) ChangePassword(ctx context.Context, oldPassword, newPassword string) error {
	if _, err := c.authc.ChangePassword(ctx, &quorumv1.ChangePasswordRequest{
		OldPassword: oldPassword,
		NewPassword: newPassword,
	}); err != nil {
		return err
	}
	c.mu.Lock()
	c.password = newPassword
	c.mu.Unlock()
	return nil
}

// relogin re-authenticates with the remembered password after the token
// was rejected during reconnect.
func (c *Client) relogin(ctx context.Context) error {
	c.mu.Lock()
	username, password := c.username, c.password
	c.mu.Unlock()
	if username == "" || password == "" {
		return errors.New("no stored credentials")
	}
	resp, err := c.authc.Login(ctx, &quorumv1.LoginRequest{Username: username, Password: password})
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.token = resp.GetToken()
	c.mu.Unlock()
	return nil
}

func (c *Client) UserID() string   { return c.lockedString(&c.userID) }
func (c *Client) Username() string { return c.lockedString(&c.username) }
func (c *Client) ServerID() string { return c.serverID }

func (c *Client) lockedString(s *string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *s
}

// IdentityFingerprint renders our own public key fingerprint for the
// status bar.
func (c *Client) IdentityFingerprint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.identity == nil {
		return ""
	}
	return e2ee.Fingerprint(c.identity.PublicKey().Bytes())
}

func (c *Client) Close() error { return c.conn.Close() }

// emit forwards an event to the UI callback, if registered.
func (c *Client) emit(ev Event) {
	if c.onEvent != nil {
		c.onEvent(ev)
	}
}

// Run pumps the Subscribe stream, reconnecting with backoff until ctx is
// cancelled. All inbound traffic - including decrypted DMs - is delivered
// through onEvent; the function blocks.
func (c *Client) Run(ctx context.Context, onEvent func(Event)) {
	c.onEvent = onEvent
	backoff := time.Second
	first := true
	for {
		if ctx.Err() != nil {
			return
		}
		if !first {
			c.emit(ConnStateEvent{State: ConnReconnecting})
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
		first = false

		stream, err := c.chatc.Subscribe(ctx, &quorumv1.SubscribeRequest{})
		if err != nil {
			if status.Code(err) == codes.Unauthenticated {
				if rerr := c.relogin(ctx); rerr == nil {
					backoff = time.Second
					continue
				}
			}
			c.emit(ConnStateEvent{State: ConnOffline, Err: err})
			continue
		}

		// Sessions cannot survive a gap in the stream (the peer may have
		// sent frames we never received); drop them, fail closed.
		c.dm.reset()

		c.emit(ConnStateEvent{State: ConnOnline})
		c.emit(ResyncEvent{})
		backoff = time.Second

		for {
			ev, err := stream.Recv()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if status.Code(err) == codes.Unauthenticated {
					_ = c.relogin(ctx)
				}
				c.emit(ConnStateEvent{State: ConnOffline, Err: err})
				break
			}
			c.dispatch(ctx, ev)
		}
	}
}

func (c *Client) dispatch(ctx context.Context, ev *quorumv1.ServerEvent) {
	switch e := ev.GetEvent().(type) {
	case *quorumv1.ServerEvent_ChannelMessage:
		c.emit(ChannelMessageEvent{Msg: e.ChannelMessage})
	case *quorumv1.ServerEvent_Presence:
		// A peer going offline will reset its E2EE state on return, so our
		// session to it is already dead: drop it now to force a fresh
		// handshake on the next send (see dmManager.peerOffline).
		if p := e.Presence; !p.GetOnline() && p.GetUserId() != c.UserID() {
			c.dm.peerOffline(p.GetUserId(), p.GetUsername())
		}
		c.emit(PresenceEvent{Presence: e.Presence})
	case *quorumv1.ServerEvent_ChannelEvent:
		c.emit(ChannelEventEvent{Event: e.ChannelEvent})
	case *quorumv1.ServerEvent_System:
		c.emit(SystemEvent{Notice: e.System})
	case *quorumv1.ServerEvent_DirectEnvelope:
		c.dm.handleEnvelope(ctx, e.DirectEnvelope)
	}
}

// --- channel operations (thin RPC wrappers) ---

func (c *Client) ListChannels(ctx context.Context) ([]*quorumv1.Channel, error) {
	resp, err := c.chatc.ListChannels(ctx, &quorumv1.ListChannelsRequest{})
	if err != nil {
		return nil, err
	}
	return resp.GetChannels(), nil
}

func (c *Client) ListUsers(ctx context.Context) ([]*quorumv1.User, error) {
	resp, err := c.chatc.ListUsers(ctx, &quorumv1.ListUsersRequest{})
	if err != nil {
		return nil, err
	}
	return resp.GetUsers(), nil
}

func (c *Client) CreateChannel(ctx context.Context, name string) (*quorumv1.Channel, error) {
	return c.chatc.CreateChannel(ctx, &quorumv1.CreateChannelRequest{Name: name})
}

func (c *Client) JoinChannel(ctx context.Context, channelID string) (*quorumv1.Channel, error) {
	resp, err := c.chatc.JoinChannel(ctx, &quorumv1.JoinChannelRequest{ChannelId: channelID})
	if err != nil {
		return nil, err
	}
	return resp.GetChannel(), nil
}

func (c *Client) LeaveChannel(ctx context.Context, channelID string) error {
	_, err := c.chatc.LeaveChannel(ctx, &quorumv1.LeaveChannelRequest{ChannelId: channelID})
	return err
}

func (c *Client) SendChannelMessage(ctx context.Context, channelID, body string) error {
	_, err := c.chatc.SendChannelMessage(ctx, &quorumv1.SendChannelMessageRequest{ChannelId: channelID, Body: body})
	return err
}

func (c *Client) ChannelHistory(ctx context.Context, channelID string, beforeID int64, limit int32) ([]*quorumv1.ChannelMessage, error) {
	resp, err := c.chatc.GetChannelHistory(ctx, &quorumv1.GetChannelHistoryRequest{
		ChannelId: channelID, BeforeId: beforeID, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetMessages(), nil
}

func (c *Client) RegisterCommands(ctx context.Context, cmds []*quorumv1.CommandSpec) ([]string, error) {
	resp, err := c.chatc.RegisterCommands(ctx, &quorumv1.RegisterCommandsRequest{Commands: cmds})
	if err != nil {
		return nil, err
	}
	return resp.GetDuplicateNames(), nil
}

// SendDM encrypts and sends text to a peer, transparently establishing an
// E2EE session first if needed (queued text is sent only after the
// handshake completes; see dmManager for the fail-closed rules).
func (c *Client) SendDM(ctx context.Context, peerID, peerName, text string) error {
	return c.dm.send(ctx, peerID, peerName, text)
}

// DMSessionInfo reports whether an E2EE session exists with peer and the
// pinned fingerprint of their identity key.
func (c *Client) DMSessionInfo(peerID string) (established bool, fingerprint string) {
	return c.dm.info(peerID)
}
