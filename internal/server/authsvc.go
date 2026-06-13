// Package server implements the gRPC services (auth, chat, admin).
package server

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	quorumv1 "github.com/clwg/quorum/gen/quorum/v1"
	"github.com/clwg/quorum/internal/auth"
	"github.com/clwg/quorum/internal/store"
)

type AuthService struct {
	quorumv1.UnimplementedAuthServiceServer
	store   *store.Store
	limiter *rateLimiter
}

func NewAuthService(st *store.Store) *AuthService {
	// Throttle login attempts per username: ~5/min sustained, burst 5.
	return &AuthService{store: st, limiter: newRateLimiter(5.0/60.0, 5)}
}

func (s *AuthService) Login(ctx context.Context, req *quorumv1.LoginRequest) (*quorumv1.LoginResponse, error) {
	if !s.limiter.allow(strings.ToLower(req.GetUsername())) {
		return nil, status.Error(codes.ResourceExhausted, "too many login attempts; slow down")
	}
	u, err := s.store.GetUserByUsername(ctx, req.GetUsername())
	if err != nil {
		// Hash anyway so missing and present users take comparable time.
		_ = auth.VerifyPassword(req.GetPassword(), dummyPHC)
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	if u.Disabled || u.Role == "bot" || u.PasswordHash == "" {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	if err := auth.VerifyPassword(req.GetPassword(), u.PasswordHash); err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}

	token := auth.NewToken(auth.SessionTokenPrefix)
	sess, err := s.store.CreateSession(ctx, auth.HashToken(token), u.ID, auth.SessionTTL)
	if err != nil {
		return nil, status.Error(codes.Internal, "could not create session")
	}
	return &quorumv1.LoginResponse{
		Token:     token,
		UserId:    u.ID,
		Username:  u.Username,
		Role:      u.Role,
		ExpiresAt: timestamppb.New(time.UnixMilli(sess.ExpiresAt)),
	}, nil
}

// dummyPHC is a hash of an unguessable random value, used to equalize
// timing when the username does not exist.
var dummyPHC = func() string {
	phc, err := auth.HashPassword(auth.NewToken("dummy_"))
	if err != nil {
		panic(err)
	}
	return phc
}()

// ChangePassword replaces the caller's own password after verifying the
// current one. Unlike the admin ResetPassword, it leaves existing sessions
// (including this one) intact, so the user is not logged out of the client
// they just changed it from.
func (s *AuthService) ChangePassword(ctx context.Context, req *quorumv1.ChangePasswordRequest) (*quorumv1.ChangePasswordResponse, error) {
	ident := auth.FromContext(ctx)
	if ident == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}
	u, err := s.store.GetUserByID(ctx, ident.UserID)
	if err != nil {
		return nil, status.Error(codes.Internal, "lookup failed")
	}
	if u.Role == "bot" || u.PasswordHash == "" {
		return nil, status.Error(codes.FailedPrecondition, "bots have tokens, not passwords")
	}
	if err := auth.VerifyPassword(req.GetOldPassword(), u.PasswordHash); err != nil {
		return nil, status.Error(codes.Unauthenticated, "current password is incorrect")
	}
	if len(req.GetNewPassword()) < 8 {
		return nil, status.Error(codes.InvalidArgument, "password must be at least 8 characters")
	}
	if req.GetNewPassword() == req.GetOldPassword() {
		return nil, status.Error(codes.InvalidArgument, "new password must differ from the current one")
	}
	phc, err := auth.HashPassword(req.GetNewPassword())
	if err != nil {
		return nil, status.Error(codes.Internal, "hash failed")
	}
	if err := s.store.SetUserPassword(ctx, u.ID, phc); err != nil {
		return nil, status.Error(codes.Internal, "update failed")
	}
	return &quorumv1.ChangePasswordResponse{}, nil
}

func (s *AuthService) WhoAmI(ctx context.Context, _ *quorumv1.WhoAmIRequest) (*quorumv1.WhoAmIResponse, error) {
	ident := auth.FromContext(ctx)
	if ident == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}
	return &quorumv1.WhoAmIResponse{UserId: ident.UserID, Username: ident.Username, Role: ident.Role}, nil
}

// Logout invalidates all of the caller's sessions.
func (s *AuthService) Logout(ctx context.Context, _ *quorumv1.LogoutRequest) (*quorumv1.LogoutResponse, error) {
	ident := auth.FromContext(ctx)
	if ident == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}
	if err := s.store.DeleteUserSessions(ctx, ident.UserID); err != nil {
		return nil, status.Error(codes.Internal, "logout failed")
	}
	return &quorumv1.LogoutResponse{}, nil
}

func (s *AuthService) PublishIdentityKey(ctx context.Context, req *quorumv1.PublishIdentityKeyRequest) (*quorumv1.PublishIdentityKeyResponse, error) {
	ident := auth.FromContext(ctx)
	if ident == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}
	key := req.GetX25519PublicKey()
	if len(key) != 32 {
		return nil, status.Error(codes.InvalidArgument, "identity key must be 32 bytes")
	}
	if err := s.store.SetIdentityKey(ctx, ident.UserID, key); err != nil {
		return nil, status.Error(codes.Internal, "could not store identity key")
	}
	return &quorumv1.PublishIdentityKeyResponse{}, nil
}

func (s *AuthService) GetIdentityKey(ctx context.Context, req *quorumv1.GetIdentityKeyRequest) (*quorumv1.GetIdentityKeyResponse, error) {
	u, err := s.store.GetUserByID(ctx, req.GetUserId())
	if err != nil {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	key, err := s.store.GetIdentityKey(ctx, u.ID)
	if err != nil && err != store.ErrNotFound {
		return nil, status.Error(codes.Internal, "lookup failed")
	}
	return &quorumv1.GetIdentityKeyResponse{
		UserId:          u.ID,
		Username:        u.Username,
		X25519PublicKey: key, // empty if unpublished
	}, nil
}
