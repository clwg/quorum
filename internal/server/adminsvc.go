package server

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	quorumv1 "github.com/layer8/quorum/gen/quorum/v1"
	"github.com/layer8/quorum/internal/auth"
	"github.com/layer8/quorum/internal/hub"
	"github.com/layer8/quorum/internal/store"
)

// AdminService manages users and bots. Every RPC here is reachable only
// by role=admin identities (enforced by the auth interceptor).
type AdminService struct {
	quorumv1.UnimplementedAdminServiceServer
	store *store.Store
	hub   *hub.Hub
}

func NewAdminService(st *store.Store, h *hub.Hub) *AdminService {
	return &AdminService{store: st, hub: h}
}

func adminUserPB(u *store.User) *quorumv1.AdminUser {
	return &quorumv1.AdminUser{
		Id:        u.ID,
		Username:  u.Username,
		Role:      u.Role,
		Disabled:  u.Disabled,
		CreatedAt: timestamppb.New(time.UnixMilli(u.CreatedAt)),
	}
}

func (s *AdminService) ListUsers(ctx context.Context, _ *quorumv1.AdminListUsersRequest) (*quorumv1.AdminListUsersResponse, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "list failed")
	}
	resp := &quorumv1.AdminListUsersResponse{}
	for _, u := range users {
		resp.Users = append(resp.Users, adminUserPB(u))
	}
	return resp, nil
}

func validUsername(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func (s *AdminService) CreateUser(ctx context.Context, req *quorumv1.CreateUserRequest) (*quorumv1.CreateUserResponse, error) {
	username := strings.TrimSpace(req.GetUsername())
	if !validUsername(username) {
		return nil, status.Error(codes.InvalidArgument, "invalid username (1-32 chars: a-z A-Z 0-9 - _ .)")
	}
	role := req.GetRole()
	if role == "" {
		role = "user"
	}
	if role != "user" && role != "admin" {
		return nil, status.Error(codes.InvalidArgument, "role must be user or admin")
	}
	if len(req.GetPassword()) < 8 {
		return nil, status.Error(codes.InvalidArgument, "password must be at least 8 characters")
	}
	phc, err := auth.HashPassword(req.GetPassword())
	if err != nil {
		return nil, status.Error(codes.Internal, "hash failed")
	}
	u := &store.User{ID: store.NewID(), Username: username, PasswordHash: phc, Role: role}
	if err := s.store.CreateUser(ctx, u); err != nil {
		return nil, status.Error(codes.AlreadyExists, "username taken")
	}
	return &quorumv1.CreateUserResponse{User: adminUserPB(u)}, nil
}

func (s *AdminService) SetUserDisabled(ctx context.Context, req *quorumv1.SetUserDisabledRequest) (*quorumv1.SetUserDisabledResponse, error) {
	if err := s.guardSelf(ctx, req.GetUserId()); err != nil {
		return nil, err
	}
	if err := s.store.SetUserDisabled(ctx, req.GetUserId(), req.GetDisabled()); err != nil {
		return nil, status.Error(codes.Internal, "update failed")
	}
	if req.GetDisabled() {
		_ = s.store.DeleteUserSessions(ctx, req.GetUserId())
		s.hub.Kick(req.GetUserId())
	}
	return &quorumv1.SetUserDisabledResponse{}, nil
}

func (s *AdminService) DeleteUser(ctx context.Context, req *quorumv1.DeleteUserRequest) (*quorumv1.DeleteUserResponse, error) {
	if err := s.guardSelf(ctx, req.GetUserId()); err != nil {
		return nil, err
	}
	if err := s.store.DeleteUser(ctx, req.GetUserId()); err != nil {
		return nil, status.Error(codes.Internal, "delete failed")
	}
	s.hub.Kick(req.GetUserId())
	return &quorumv1.DeleteUserResponse{}, nil
}

// guardSelf prevents an admin from disabling or deleting themselves.
func (s *AdminService) guardSelf(ctx context.Context, targetID string) error {
	ident := auth.FromContext(ctx)
	if ident != nil && ident.UserID == targetID {
		return status.Error(codes.FailedPrecondition, "cannot modify your own account")
	}
	return nil
}

func (s *AdminService) ResetPassword(ctx context.Context, req *quorumv1.ResetPasswordRequest) (*quorumv1.ResetPasswordResponse, error) {
	if len(req.GetNewPassword()) < 8 {
		return nil, status.Error(codes.InvalidArgument, "password must be at least 8 characters")
	}
	u, err := s.store.GetUserByID(ctx, req.GetUserId())
	if err != nil {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	if u.Role == "bot" {
		return nil, status.Error(codes.InvalidArgument, "bots have tokens, not passwords")
	}
	phc, err := auth.HashPassword(req.GetNewPassword())
	if err != nil {
		return nil, status.Error(codes.Internal, "hash failed")
	}
	if err := s.store.SetUserPassword(ctx, u.ID, phc); err != nil {
		return nil, status.Error(codes.Internal, "update failed")
	}
	// Force re-login everywhere with the new password.
	_ = s.store.DeleteUserSessions(ctx, u.ID)
	s.hub.Kick(u.ID)
	return &quorumv1.ResetPasswordResponse{}, nil
}

func (s *AdminService) CreateBot(ctx context.Context, req *quorumv1.CreateBotRequest) (*quorumv1.CreateBotResponse, error) {
	ident := auth.FromContext(ctx)
	username := strings.TrimSpace(req.GetUsername())
	if !validUsername(username) {
		return nil, status.Error(codes.InvalidArgument, "invalid bot username")
	}
	u := &store.User{ID: store.NewID(), Username: username, Role: "bot"}
	if err := s.store.CreateUser(ctx, u); err != nil {
		return nil, status.Error(codes.AlreadyExists, "username taken")
	}
	token := auth.NewToken(auth.BotTokenPrefix)
	b := &store.Bot{UserID: u.ID, OwnerID: ident.UserID, TokenHash: auth.HashToken(token)}
	if err := s.store.CreateBot(ctx, b); err != nil {
		_ = s.store.DeleteUser(ctx, u.ID)
		return nil, status.Error(codes.Internal, "could not create bot")
	}
	return &quorumv1.CreateBotResponse{
		Bot: &quorumv1.Bot{
			UserId:    u.ID,
			Username:  u.Username,
			OwnerId:   ident.UserID,
			OwnerName: ident.Username,
			CreatedAt: timestamppb.New(time.UnixMilli(b.CreatedAt)),
		},
		Token: token,
	}, nil
}

func (s *AdminService) ListBots(ctx context.Context, _ *quorumv1.ListBotsRequest) (*quorumv1.ListBotsResponse, error) {
	bots, err := s.store.ListBots(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "list failed")
	}
	resp := &quorumv1.ListBotsResponse{}
	for _, b := range bots {
		pb := &quorumv1.Bot{
			UserId:    b.UserID,
			OwnerId:   b.OwnerID,
			CreatedAt: timestamppb.New(time.UnixMilli(b.CreatedAt)),
		}
		if u, err := s.store.GetUserByID(ctx, b.UserID); err == nil {
			pb.Username = u.Username
		}
		if o, err := s.store.GetUserByID(ctx, b.OwnerID); err == nil {
			pb.OwnerName = o.Username
		}
		resp.Bots = append(resp.Bots, pb)
	}
	return resp, nil
}

func (s *AdminService) RevokeBotToken(ctx context.Context, req *quorumv1.RevokeBotTokenRequest) (*quorumv1.RevokeBotTokenResponse, error) {
	u, err := s.store.GetUserByID(ctx, req.GetUserId())
	if err != nil || u.Role != "bot" {
		return nil, status.Error(codes.NotFound, "bot not found")
	}
	token := auth.NewToken(auth.BotTokenPrefix)
	if err := s.store.SetBotTokenHash(ctx, u.ID, auth.HashToken(token)); err != nil {
		return nil, status.Error(codes.Internal, "rotate failed")
	}
	s.hub.Kick(u.ID)
	return &quorumv1.RevokeBotTokenResponse{NewToken: token}, nil
}
