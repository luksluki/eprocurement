package token

import (
	"context"
	"crypto/ed25519"

	identityv1 "github.com/luksluki/eprocurement/gen/go/identity/v1"
)

// grpcFetcher loads the key set from the identity service's GetJwks RPC.
type grpcFetcher struct {
	client identityv1.IdentityClient
}

// NewGRPCFetcher returns a Fetcher backed by the identity gRPC client.
func NewGRPCFetcher(client identityv1.IdentityClient) Fetcher {
	return &grpcFetcher{client: client}
}

func (f *grpcFetcher) FetchKeys(ctx context.Context) ([]Key, error) {
	resp, err := f.client.GetJwks(ctx, &identityv1.GetJwksRequest{})
	if err != nil {
		return nil, err
	}
	keys := make([]Key, 0, len(resp.GetKeys()))
	for _, k := range resp.GetKeys() {
		if len(k.GetEd25519()) != 32 || k.GetKid() == "" {
			continue // malformed entry: skip rather than fail the whole set
		}
		keys = append(keys, Key{
			Kid:     k.GetKid(),
			Ed25519: ed25519.PublicKey(k.GetEd25519()),
			Current: k.GetCurrent(),
		})
	}
	if len(keys) == 0 {
		return nil, ErrInvalidToken
	}
	return keys, nil
}
