package auth

import (
	"context"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/clwg/quorum/internal/store"
)

// SessionTTL is the sliding expiry window for user sessions.
const SessionTTL = 7 * 24 * time.Hour

// Identity is the authenticated caller, injected into the request context.
type Identity struct {
	UserID   string
	Username string
	Role     string // user | admin | bot
}

type ctxKey struct{}

// FromContext returns the authenticated identity, or nil for exempt methods.
func FromContext(ctx context.Context) *Identity {
	id, _ := ctx.Value(ctxKey{}).(*Identity)
	return id
}

// exemptMethods may be called without a bearer token.
var exemptMethods = map[string]bool{
	"/quorum.v1.AuthService/Login": true,
}

// Authenticator resolves bearer tokens to identities and enforces role
// gates. It also lazily extends session expiry (at most once per minute
// per token to avoid a DB write on every RPC).
type Authenticator struct {
	store *store.Store

	mu          sync.Mutex
	lastTouched map[string]time.Time // token-hash hex -> last touch
}

func NewAuthenticator(st *store.Store) *Authenticator {
	return &Authenticator{store: st, lastTouched: make(map[string]time.Time)}
}

func (a *Authenticator) resolve(ctx context.Context, fullMethod string) (context.Context, error) {
	if exemptMethods[fullMethod] {
		return ctx, nil
	}
	md, _ := metadata.FromIncomingContext(ctx)
	var token string
	if vals := md.Get("authorization"); len(vals) > 0 {
		token = strings.TrimPrefix(vals[0], "Bearer ")
	}
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "missing bearer token")
	}

	hash := HashToken(token)
	var ident *Identity
	if IsBotToken(token) {
		bot, err := a.store.GetBotByTokenHash(ctx, hash)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		u, err := a.store.GetUserByID(ctx, bot.UserID)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		if u.Disabled {
			return nil, status.Error(codes.Unauthenticated, "account disabled")
		}
		ident = &Identity{UserID: u.ID, Username: u.Username, Role: u.Role}
	} else {
		sess, err := a.store.GetSession(ctx, hash)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		if time.Now().UnixMilli() > sess.ExpiresAt {
			return nil, status.Error(codes.Unauthenticated, "session expired")
		}
		u, err := a.store.GetUserByID(ctx, sess.UserID)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		if u.Disabled {
			return nil, status.Error(codes.Unauthenticated, "account disabled")
		}
		a.maybeTouch(ctx, hash)
		ident = &Identity{UserID: u.ID, Username: u.Username, Role: u.Role}
	}

	if strings.HasPrefix(fullMethod, "/quorum.v1.AdminService/") && ident.Role != "admin" {
		return nil, status.Error(codes.PermissionDenied, "admin role required")
	}
	return context.WithValue(ctx, ctxKey{}, ident), nil
}

func (a *Authenticator) maybeTouch(ctx context.Context, hash []byte) {
	key := string(hash)
	a.mu.Lock()
	last, ok := a.lastTouched[key]
	now := time.Now()
	if ok && now.Sub(last) < time.Minute {
		a.mu.Unlock()
		return
	}
	a.lastTouched[key] = now
	a.mu.Unlock()
	_ = a.store.TouchSession(ctx, hash, SessionTTL)
}

func (a *Authenticator) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := a.resolve(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func (a *Authenticator) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := a.resolve(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
	}
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }
