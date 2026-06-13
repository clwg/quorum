package server_test

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	quorumv1 "github.com/layer8/quorum/gen/quorum/v1"
	"github.com/layer8/quorum/internal/auth"
	"github.com/layer8/quorum/internal/hub"
	"github.com/layer8/quorum/internal/server"
	"github.com/layer8/quorum/internal/store"
)

type testEnv struct {
	store *store.Store
	hub   *hub.Hub
	conn  *grpc.ClientConn
	auth  quorumv1.AuthServiceClient
	chat  quorumv1.ChatServiceClient
	admin quorumv1.AdminServiceClient
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	h := hub.New()
	authn := auth.NewAuthenticator(st)
	logger := slog.Default()

	srv := grpc.NewServer(
		grpc.UnaryInterceptor(authn.UnaryInterceptor()),
		grpc.StreamInterceptor(authn.StreamInterceptor()),
	)
	quorumv1.RegisterAuthServiceServer(srv, server.NewAuthService(st))
	quorumv1.RegisterChatServiceServer(srv, server.NewChatService(st, h, logger))
	quorumv1.RegisterAdminServiceServer(srv, server.NewAdminService(st, h))

	lis := bufconn.Listen(1 << 20)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	return &testEnv{
		store: st, hub: h, conn: conn,
		auth:  quorumv1.NewAuthServiceClient(conn),
		chat:  quorumv1.NewChatServiceClient(conn),
		admin: quorumv1.NewAdminServiceClient(conn),
	}
}

func (e *testEnv) createUser(t *testing.T, username, password, role string) string {
	t.Helper()
	phc, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	u := &store.User{ID: store.NewID(), Username: username, PasswordHash: phc, Role: role}
	if err := e.store.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func (e *testEnv) login(t *testing.T, username, password string) (string, context.Context) {
	t.Helper()
	resp, err := e.auth.Login(context.Background(), &quorumv1.LoginRequest{Username: username, Password: password})
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	return resp.Token, authedCtx(resp.Token)
}

func authedCtx(token string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
}

func TestLoginAndAuthGating(t *testing.T) {
	e := newTestEnv(t)
	e.createUser(t, "alice", "password123", "user")

	// Wrong password.
	_, err := e.auth.Login(context.Background(), &quorumv1.LoginRequest{Username: "alice", Password: "nope"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
	// Unknown user.
	_, err = e.auth.Login(context.Background(), &quorumv1.LoginRequest{Username: "ghost", Password: "nope"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
	// Missing token on an authed RPC.
	_, err = e.chat.ListChannels(context.Background(), &quorumv1.ListChannelsRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
	// Garbage token.
	_, err = e.chat.ListChannels(authedCtx("qsess_garbage"), &quorumv1.ListChannelsRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}

	// Valid login works end-to-end.
	_, ctx := e.login(t, "alice", "password123")
	if _, err := e.chat.ListChannels(ctx, &quorumv1.ListChannelsRequest{}); err != nil {
		t.Fatalf("authed call failed: %v", err)
	}

	// Non-admin denied on AdminService.
	_, err = e.admin.ListUsers(ctx, &quorumv1.AdminListUsersRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
}

func TestDisabledUserRejected(t *testing.T) {
	e := newTestEnv(t)
	id := e.createUser(t, "bob", "password123", "user")
	_, ctx := e.login(t, "bob", "password123")

	if err := e.store.SetUserDisabled(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	_, err := e.chat.ListChannels(ctx, &quorumv1.ListChannelsRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("disabled user should be rejected, got %v", err)
	}
}

// subscribeCollect opens a Subscribe stream and forwards events to a channel.
func subscribeCollect(t *testing.T, chat quorumv1.ChatServiceClient, ctx context.Context) <-chan *quorumv1.ServerEvent {
	t.Helper()
	stream, err := chat.Subscribe(ctx, &quorumv1.SubscribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// First event is the "connected" system notice.
	first, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if first.GetSystem() == nil {
		t.Fatalf("expected system notice first, got %v", first)
	}
	out := make(chan *quorumv1.ServerEvent, 64)
	go func() {
		defer close(out)
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			out <- ev
		}
	}()
	return out
}

func waitFor[T any](t *testing.T, ch <-chan *quorumv1.ServerEvent, match func(*quorumv1.ServerEvent) (T, bool)) T {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("stream closed while waiting")
			}
			if v, ok := match(ev); ok {
				return v
			}
		case <-deadline:
			t.Fatal("timed out waiting for event")
		}
	}
}

func TestChannelFlow(t *testing.T) {
	e := newTestEnv(t)
	e.createUser(t, "alice", "password123", "user")
	e.createUser(t, "bob", "password123", "user")
	e.createUser(t, "carol", "password123", "user")

	_, aCtx := e.login(t, "alice", "password123")
	_, bCtx := e.login(t, "bob", "password123")
	_, cCtx := e.login(t, "carol", "password123")

	aEvents := subscribeCollect(t, e.chat, aCtx)
	bEvents := subscribeCollect(t, e.chat, bCtx)
	cEvents := subscribeCollect(t, e.chat, cCtx)

	ch, err := e.chat.CreateChannel(aCtx, &quorumv1.CreateChannelRequest{Name: "general"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.chat.JoinChannel(bCtx, &quorumv1.JoinChannelRequest{ChannelId: ch.Id}); err != nil {
		t.Fatal(err)
	}

	// Carol is not a member: send must be denied.
	_, err = e.chat.SendChannelMessage(cCtx, &quorumv1.SendChannelMessageRequest{ChannelId: ch.Id, Body: "hi"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-member send: want PermissionDenied, got %v", err)
	}
	// And history denied.
	_, err = e.chat.GetChannelHistory(cCtx, &quorumv1.GetChannelHistoryRequest{ChannelId: ch.Id})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-member history: want PermissionDenied, got %v", err)
	}

	// Alice sends; both alice (echo) and bob receive, in order.
	for _, body := range []string{"one", "two", "three"} {
		if _, err := e.chat.SendChannelMessage(aCtx, &quorumv1.SendChannelMessageRequest{ChannelId: ch.Id, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	for _, events := range []<-chan *quorumv1.ServerEvent{aEvents, bEvents} {
		var got []string
		for len(got) < 3 {
			msg := waitFor(t, events, func(ev *quorumv1.ServerEvent) (*quorumv1.ChannelMessage, bool) {
				m := ev.GetChannelMessage()
				return m, m != nil
			})
			got = append(got, msg.Body)
		}
		if got[0] != "one" || got[1] != "two" || got[2] != "three" {
			t.Fatalf("out of order: %v", got)
		}
	}
	// Carol must not have received any channel message.
	select {
	case ev := <-cEvents:
		if ev.GetChannelMessage() != nil {
			t.Fatalf("non-member received channel message: %v", ev)
		}
	default:
	}

	// History pagination: page of 2 then the rest.
	hist, err := e.chat.GetChannelHistory(aCtx, &quorumv1.GetChannelHistoryRequest{ChannelId: ch.Id, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.Messages) != 2 || hist.Messages[0].Body != "two" || hist.Messages[1].Body != "three" {
		t.Fatalf("unexpected latest page: %+v", hist.Messages)
	}
	hist2, err := e.chat.GetChannelHistory(aCtx, &quorumv1.GetChannelHistoryRequest{ChannelId: ch.Id, BeforeId: hist.Messages[0].Id, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hist2.Messages) != 1 || hist2.Messages[0].Body != "one" {
		t.Fatalf("unexpected older page: %+v", hist2.Messages)
	}
}

func TestPresence(t *testing.T) {
	e := newTestEnv(t)
	aliceID := e.createUser(t, "alice", "password123", "user")
	e.createUser(t, "bob", "password123", "user")

	_, aCtx := e.login(t, "alice", "password123")
	_, bCtx := e.login(t, "bob", "password123")

	bEvents := subscribeCollect(t, e.chat, bCtx)

	aCtxCancel, cancel := context.WithCancel(aCtx)
	stream, err := e.chat.Subscribe(aCtxCancel, &quorumv1.SubscribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil { // consume system notice
		t.Fatal(err)
	}

	online := waitFor(t, bEvents, func(ev *quorumv1.ServerEvent) (*quorumv1.PresenceEvent, bool) {
		p := ev.GetPresence()
		return p, p != nil && p.UserId == aliceID && p.Online
	})
	if online.Username != "alice" {
		t.Fatalf("unexpected presence: %+v", online)
	}

	cancel() // alice disconnects
	waitFor(t, bEvents, func(ev *quorumv1.ServerEvent) (*quorumv1.PresenceEvent, bool) {
		p := ev.GetPresence()
		return p, p != nil && p.UserId == aliceID && !p.Online
	})
}

func TestSendDirectRequiresOnlineRecipient(t *testing.T) {
	e := newTestEnv(t)
	e.createUser(t, "alice", "password123", "user")
	bobID := e.createUser(t, "bob", "password123", "user")

	_, aCtx := e.login(t, "alice", "password123")
	_, err := e.chat.SendDirect(aCtx, &quorumv1.SendDirectRequest{Envelope: &quorumv1.DirectEnvelope{
		Type:        quorumv1.DirectEnvelope_TYPE_SESSION_INIT,
		RecipientId: bobID,
		Payload:     make([]byte, 32),
	}})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("want Unavailable for offline recipient, got %v", err)
	}

	// Bob comes online; envelope is relayed with server-set sender identity.
	_, bCtx := e.login(t, "bob", "password123")
	bEvents := subscribeCollect(t, e.chat, bCtx)
	if _, err := e.chat.SendDirect(aCtx, &quorumv1.SendDirectRequest{Envelope: &quorumv1.DirectEnvelope{
		Type:        quorumv1.DirectEnvelope_TYPE_SESSION_INIT,
		RecipientId: bobID,
		SenderId:    "spoofed-id", // must be overwritten
		Payload:     make([]byte, 32),
	}}); err != nil {
		t.Fatal(err)
	}
	env := waitFor(t, bEvents, func(ev *quorumv1.ServerEvent) (*quorumv1.DirectEnvelope, bool) {
		d := ev.GetDirectEnvelope()
		return d, d != nil
	})
	if env.SenderName != "alice" || env.SenderId == "spoofed-id" {
		t.Fatalf("sender identity not enforced: %+v", env)
	}
}
