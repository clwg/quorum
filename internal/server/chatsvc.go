package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	quorumv1 "github.com/layer8/quorum/gen/quorum/v1"
	"github.com/layer8/quorum/internal/auth"
	"github.com/layer8/quorum/internal/hub"
	"github.com/layer8/quorum/internal/store"
)

const (
	maxMessageBytes   = 4096
	maxHistoryLimit   = 200
	defaultHistory    = 50
	maxChannelNameLen = 64
)

type ChatService struct {
	quorumv1.UnimplementedChatServiceServer
	store  *store.Store
	hub    *hub.Hub
	logger *slog.Logger

	cmdMu    sync.RWMutex
	commands map[string]commandOwner // command name -> owning bot

	directLimiter *rateLimiter // per-sender throttle on SendDirect
}

type commandOwner struct {
	botID   string
	botName string
	help    string
}

func NewChatService(st *store.Store, h *hub.Hub, logger *slog.Logger) *ChatService {
	return &ChatService{
		store: st, hub: h, logger: logger,
		commands:      make(map[string]commandOwner),
		directLimiter: newRateLimiter(20, 40), // 20 envelopes/sec/sender, burst 40
	}
}

func mustIdentity(ctx context.Context) (*auth.Identity, error) {
	ident := auth.FromContext(ctx)
	if ident == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}
	return ident, nil
}

// Subscribe is the realtime event stream. One per user; registering kicks
// any previous stream for the same user.
func (s *ChatService) Subscribe(_ *quorumv1.SubscribeRequest, stream quorumv1.ChatService_SubscribeServer) error {
	ctx := stream.Context()
	ident, err := mustIdentity(ctx)
	if err != nil {
		return err
	}

	sub := s.hub.Register(ident.UserID, ident.Username)
	defer func() {
		s.hub.Unregister(sub)
		s.hub.Broadcast(presenceEvent(ident, false))
	}()
	s.hub.Broadcast(presenceEvent(ident, true))

	// Initial sync notice.
	if err := stream.Send(&quorumv1.ServerEvent{Event: &quorumv1.ServerEvent_System{
		System: &quorumv1.SystemNotice{
			Text:       "connected",
			ServerTime: timestamppb.Now(),
		},
	}}); err != nil {
		return err
	}

	for {
		select {
		case ev := <-sub.Ch:
			if err := stream.Send(ev); err != nil {
				return err
			}
		case <-sub.Done:
			return status.Error(codes.ResourceExhausted, "stream replaced or terminated")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func presenceEvent(ident *auth.Identity, online bool) *quorumv1.ServerEvent {
	return &quorumv1.ServerEvent{Event: &quorumv1.ServerEvent_Presence{
		Presence: &quorumv1.PresenceEvent{UserId: ident.UserID, Username: ident.Username, Online: online},
	}}
}

func (s *ChatService) SendChannelMessage(ctx context.Context, req *quorumv1.SendChannelMessageRequest) (*quorumv1.SendChannelMessageResponse, error) {
	ident, err := mustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	body := req.GetBody()
	if body == "" || len(body) > maxMessageBytes {
		return nil, status.Error(codes.InvalidArgument, "message empty or too large")
	}
	member, err := s.store.IsChannelMember(ctx, req.GetChannelId(), ident.UserID)
	if err != nil {
		return nil, status.Error(codes.Internal, "membership check failed")
	}
	if !member {
		return nil, status.Error(codes.PermissionDenied, "not a channel member")
	}

	msg := &store.Message{ChannelID: req.GetChannelId(), SenderID: ident.UserID, Body: body}
	if err := s.store.InsertMessage(ctx, msg); err != nil {
		return nil, status.Error(codes.Internal, "could not store message")
	}

	memberIDs, err := s.store.ChannelMemberIDs(ctx, req.GetChannelId())
	if err != nil {
		return nil, status.Error(codes.Internal, "member lookup failed")
	}
	// Fan out to all members including the sender: clients render their own
	// messages from the stream echo, keeping a single rendering path.
	s.hub.FanOut(memberIDs, &quorumv1.ServerEvent{Event: &quorumv1.ServerEvent_ChannelMessage{
		ChannelMessage: &quorumv1.ChannelMessage{
			Id:         msg.ID,
			ChannelId:  msg.ChannelID,
			SenderId:   ident.UserID,
			SenderName: ident.Username,
			Body:       body,
			SentAt:     timestamppb.New(time.UnixMilli(msg.CreatedAt)),
		},
	}})

	// Built-in /commands: answer the sender with the registered bot commands.
	if body == "/commands" || strings.HasPrefix(body, "/commands ") {
		s.hub.SendToUser(ident.UserID, &quorumv1.ServerEvent{Event: &quorumv1.ServerEvent_System{
			System: &quorumv1.SystemNotice{Text: s.BotCommandsText(), ServerTime: timestamppb.Now()},
		}})
	}
	return &quorumv1.SendChannelMessageResponse{MessageId: msg.ID}, nil
}

func (s *ChatService) CreateChannel(ctx context.Context, req *quorumv1.CreateChannelRequest) (*quorumv1.Channel, error) {
	ident, err := mustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(strings.TrimPrefix(req.GetName(), "#"))
	if name == "" || len(name) > maxChannelNameLen || strings.ContainsAny(name, " \t\n") {
		return nil, status.Error(codes.InvalidArgument, "invalid channel name")
	}
	ch := &store.Channel{ID: store.NewID(), Name: name, Topic: req.GetTopic(), CreatedBy: ident.UserID}
	if err := s.store.CreateChannel(ctx, ch); err != nil {
		return nil, status.Error(codes.AlreadyExists, "channel name taken")
	}
	if err := s.store.AddChannelMember(ctx, ch.ID, ident.UserID); err != nil {
		return nil, status.Error(codes.Internal, "could not join created channel")
	}
	pb := channelPB(ch)
	pb.IsMember = true
	s.hub.Broadcast(&quorumv1.ServerEvent{Event: &quorumv1.ServerEvent_ChannelEvent{
		ChannelEvent: &quorumv1.ChannelEvent{
			Type:     quorumv1.ChannelEvent_TYPE_CREATED,
			Channel:  channelPB(ch),
			UserId:   ident.UserID,
			Username: ident.Username,
		},
	}})
	return pb, nil
}

func channelPB(c *store.Channel) *quorumv1.Channel {
	return &quorumv1.Channel{
		Id:        c.ID,
		Name:      c.Name,
		Topic:     c.Topic,
		IsMember:  c.IsMember,
		CreatedAt: timestamppb.New(time.UnixMilli(c.CreatedAt)),
	}
}

func (s *ChatService) JoinChannel(ctx context.Context, req *quorumv1.JoinChannelRequest) (*quorumv1.JoinChannelResponse, error) {
	ident, err := mustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := s.store.GetChannel(ctx, req.GetChannelId())
	if err != nil {
		return nil, status.Error(codes.NotFound, "channel not found")
	}
	if err := s.store.AddChannelMember(ctx, ch.ID, ident.UserID); err != nil {
		return nil, status.Error(codes.Internal, "join failed")
	}
	s.notifyChannel(ctx, ch, quorumv1.ChannelEvent_TYPE_MEMBER_JOINED, ident)
	ch.IsMember = true
	return &quorumv1.JoinChannelResponse{Channel: channelPB(ch)}, nil
}

func (s *ChatService) LeaveChannel(ctx context.Context, req *quorumv1.LeaveChannelRequest) (*quorumv1.LeaveChannelResponse, error) {
	ident, err := mustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := s.store.GetChannel(ctx, req.GetChannelId())
	if err != nil {
		return nil, status.Error(codes.NotFound, "channel not found")
	}
	if err := s.store.RemoveChannelMember(ctx, ch.ID, ident.UserID); err != nil {
		return nil, status.Error(codes.Internal, "leave failed")
	}
	s.notifyChannel(ctx, ch, quorumv1.ChannelEvent_TYPE_MEMBER_LEFT, ident)
	return &quorumv1.LeaveChannelResponse{}, nil
}

// notifyChannel fans a membership event out to current channel members.
func (s *ChatService) notifyChannel(ctx context.Context, ch *store.Channel, typ quorumv1.ChannelEvent_Type, ident *auth.Identity) {
	memberIDs, err := s.store.ChannelMemberIDs(ctx, ch.ID)
	if err != nil {
		s.logger.Warn("member lookup for notify failed", "err", err)
		return
	}
	s.hub.FanOut(memberIDs, &quorumv1.ServerEvent{Event: &quorumv1.ServerEvent_ChannelEvent{
		ChannelEvent: &quorumv1.ChannelEvent{
			Type:     typ,
			Channel:  channelPB(ch),
			UserId:   ident.UserID,
			Username: ident.Username,
		},
	}})
}

func (s *ChatService) ListChannels(ctx context.Context, _ *quorumv1.ListChannelsRequest) (*quorumv1.ListChannelsResponse, error) {
	ident, err := mustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	chans, err := s.store.ListChannelsForUser(ctx, ident.UserID)
	if err != nil {
		return nil, status.Error(codes.Internal, "list failed")
	}
	resp := &quorumv1.ListChannelsResponse{}
	for _, c := range chans {
		resp.Channels = append(resp.Channels, channelPB(c))
	}
	return resp, nil
}

func (s *ChatService) GetChannelHistory(ctx context.Context, req *quorumv1.GetChannelHistoryRequest) (*quorumv1.GetChannelHistoryResponse, error) {
	ident, err := mustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	member, err := s.store.IsChannelMember(ctx, req.GetChannelId(), ident.UserID)
	if err != nil {
		return nil, status.Error(codes.Internal, "membership check failed")
	}
	if !member {
		return nil, status.Error(codes.PermissionDenied, "not a channel member")
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultHistory
	}
	if limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}
	msgs, err := s.store.ChannelHistory(ctx, req.GetChannelId(), req.GetBeforeId(), limit)
	if err != nil {
		return nil, status.Error(codes.Internal, "history failed")
	}
	resp := &quorumv1.GetChannelHistoryResponse{}
	for _, m := range msgs {
		resp.Messages = append(resp.Messages, &quorumv1.ChannelMessage{
			Id:         m.ID,
			ChannelId:  m.ChannelID,
			SenderId:   m.SenderID,
			SenderName: m.SenderName,
			Body:       m.Body,
			SentAt:     timestamppb.New(time.UnixMilli(m.CreatedAt)),
		})
	}
	return resp, nil
}

func (s *ChatService) ListUsers(ctx context.Context, _ *quorumv1.ListUsersRequest) (*quorumv1.ListUsersResponse, error) {
	if _, err := mustIdentity(ctx); err != nil {
		return nil, err
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "list failed")
	}
	online := s.hub.OnlineIDs()
	resp := &quorumv1.ListUsersResponse{}
	for _, u := range users {
		if u.Disabled {
			continue
		}
		resp.Users = append(resp.Users, &quorumv1.User{
			Id:       u.ID,
			Username: u.Username,
			Role:     u.Role,
			Online:   online[u.ID],
		})
	}
	return resp, nil
}

// SendDirect relays an opaque E2EE envelope. Nothing is persisted; the
// payload is never inspected.
func (s *ChatService) SendDirect(ctx context.Context, req *quorumv1.SendDirectRequest) (*quorumv1.SendDirectResponse, error) {
	ident, err := mustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if !s.directLimiter.allow(ident.UserID) {
		return nil, status.Error(codes.ResourceExhausted, "sending too fast")
	}
	env := req.GetEnvelope()
	if env == nil || env.GetRecipientId() == "" {
		return nil, status.Error(codes.InvalidArgument, "missing envelope or recipient")
	}
	if len(env.GetPayload()) > maxMessageBytes*2 {
		return nil, status.Error(codes.InvalidArgument, "payload too large")
	}
	// Never trust client-supplied sender identity.
	env.SenderId = ident.UserID
	env.SenderName = ident.Username

	if s.logger.Enabled(ctx, slog.LevelDebug) {
		s.logger.Debug("relay direct envelope",
			"type", env.GetType().String(),
			"from", ident.Username,
			"to", env.GetRecipientId(),
			"payload_b64", base64.StdEncoding.EncodeToString(env.GetPayload()))
	}

	ok := s.hub.SendToUser(env.GetRecipientId(), &quorumv1.ServerEvent{
		Event: &quorumv1.ServerEvent_DirectEnvelope{DirectEnvelope: env},
	})
	if !ok {
		return nil, status.Error(codes.Unavailable, "user offline — direct messages require both parties online")
	}
	return &quorumv1.SendDirectResponse{}, nil
}

// RegisterCommands records a bot's slash commands for /commands discovery.
// Routing is client-side (the bot SDK dispatches by prefix).
func (s *ChatService) RegisterCommands(ctx context.Context, req *quorumv1.RegisterCommandsRequest) (*quorumv1.RegisterCommandsResponse, error) {
	ident, err := mustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if ident.Role != "bot" {
		return nil, status.Error(codes.PermissionDenied, "only bots may register commands")
	}
	resp := &quorumv1.RegisterCommandsResponse{}
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	// Drop this bot's previous registrations (reconnect re-registers).
	for name, owner := range s.commands {
		if owner.botID == ident.UserID {
			delete(s.commands, name)
		}
	}
	for _, c := range req.GetCommands() {
		name := strings.TrimPrefix(strings.ToLower(c.GetName()), "/")
		if name == "" {
			continue
		}
		if owner, taken := s.commands[name]; taken && owner.botID != ident.UserID {
			resp.DuplicateNames = append(resp.DuplicateNames, name)
			s.logger.Warn("duplicate command registration", "command", name, "bot", ident.Username, "owner", owner.botName)
			continue
		}
		s.commands[name] = commandOwner{botID: ident.UserID, botName: ident.Username, help: c.GetHelp()}
	}
	return resp, nil
}

// BotCommandsText renders the registered command list (used by the built-in
// /commands lookup).
func (s *ChatService) BotCommandsText() string {
	s.cmdMu.RLock()
	defer s.cmdMu.RUnlock()
	if len(s.commands) == 0 {
		return "no bot commands registered"
	}
	var b strings.Builder
	for name, owner := range s.commands {
		fmt.Fprintf(&b, "/%s — %s (%s)\n", name, owner.help, owner.botName)
	}
	return strings.TrimRight(b.String(), "\n")
}
