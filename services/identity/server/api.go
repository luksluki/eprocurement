package server

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	identityv1 "github.com/luksluki/eprocurement/gen/go/identity/v1"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// api adapts auth.Service to the identity.v1 gRPC contract. The gateway maps
// Unauthenticated to HTTP 401 — these messages are the only client-visible
// auth failure text, so they stay generic (no user enumeration).
type api struct {
	identityv1.UnimplementedIdentityServer
	svc *auth.Service
	log *slog.Logger
}

func newAPI(log *slog.Logger, svc *auth.Service) *api {
	return &api{svc: svc, log: log}
}

func (a *api) Login(ctx context.Context, req *identityv1.LoginRequest) (*identityv1.LoginResponse, error) {
	pair, err := a.svc.Login(ctx, req.GetEmail(), req.GetPassword(), rateKey(ctx, req.GetEmail()))
	if err != nil {
		return nil, mapAuthError(err)
	}
	return pairResponse(pair), nil
}

func (a *api) Refresh(ctx context.Context, req *identityv1.RefreshRequest) (*identityv1.LoginResponse, error) {
	pair, err := a.svc.Refresh(ctx, req.GetRefreshToken())
	if err != nil {
		return nil, mapAuthError(err)
	}
	return pairResponse(pair), nil
}

func (a *api) Logout(ctx context.Context, req *identityv1.LogoutRequest) (*identityv1.LogoutResponse, error) {
	if err := a.svc.Logout(ctx, req.GetRefreshToken()); err != nil {
		return nil, status.Error(codes.Internal, "logout failed")
	}
	return &identityv1.LogoutResponse{}, nil
}

func mapAuthError(err error) error {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return status.Error(codes.Unauthenticated, auth.ErrInvalidCredentials.Error())
	case errors.Is(err, auth.ErrRateLimited):
		return status.Error(codes.ResourceExhausted, auth.ErrRateLimited.Error())
	case errors.Is(err, auth.ErrInvalidRefreshToken):
		return status.Error(codes.Unauthenticated, auth.ErrInvalidRefreshToken.Error())
	default:
		return status.Error(codes.Internal, "internal error")
	}
}

func pairResponse(p auth.TokenPair) *identityv1.LoginResponse {
	return &identityv1.LoginResponse{
		AccessToken:      p.AccessToken,
		AccessExpiresIn:  p.AccessExpiresIn,
		RefreshToken:     p.RefreshToken,
		RefreshExpiresIn: p.RefreshExpiresIn,
		TokenType:        "Bearer",
	}
}

// rateKey scopes the login limiter to one email per client address.
func rateKey(ctx context.Context, email string) string {
	addr := "unknown"
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr = p.Addr.String()
	}
	return email + "|" + addr
}
