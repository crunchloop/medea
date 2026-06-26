// Package auth provides the v1 authentication for the Medea gRPC server: a
// shared bearer token checked by server interceptors (design/api-and-auth.md §4).
// Transport security (TLS) is configured separately by the server.
package auth

import (
	"context"
	"crypto/subtle"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const headerAuthorization = "authorization"

// check validates the bearer token in the request metadata against want using a
// constant-time comparison (so a wrong token can't be probed byte-by-byte).
func check(ctx context.Context, want string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing request metadata")
	}
	vals := md.Get(headerAuthorization)
	if len(vals) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization token")
	}
	expected := "Bearer " + want
	if subtle.ConstantTimeCompare([]byte(vals[0]), []byte(expected)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid authorization token")
	}
	return nil
}

// UnaryInterceptor authenticates unary RPCs against token.
func UnaryInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := check(ctx, token); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor authenticates streaming RPCs against token.
func StreamInterceptor(token string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := check(ss.Context(), token); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}
