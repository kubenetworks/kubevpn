package exitcode

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestFromError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, Success},
		{"ok", status.Error(codes.OK, ""), Success},
		{"canceled", status.Error(codes.Canceled, "interrupted"), Interrupted},
		{"invalid argument", status.Error(codes.InvalidArgument, "bad flag"), BadConfig},
		{"failed precondition", status.Error(codes.FailedPrecondition, "not ready"), BadConfig},
		{"out of range", status.Error(codes.OutOfRange, "range"), BadConfig},
		{"permission denied", status.Error(codes.PermissionDenied, "not root"), Permission},
		{"unauthenticated", status.Error(codes.Unauthenticated, "bad creds"), Permission},
		{"unavailable", status.Error(codes.Unavailable, "cluster down"), ClusterNetwork},
		{"deadline exceeded", status.Error(codes.DeadlineExceeded, "timeout"), ClusterNetwork},
		{"not found", status.Error(codes.NotFound, "missing"), NotFound},
		{"internal", status.Error(codes.Internal, "panic"), Generic},
		{"unknown", status.Error(codes.Unknown, "?"), Generic},
		{"plain go error", errors.New("boom"), Generic},
		{"wrapped plain error", fmt.Errorf("ctx: %w", errors.New("boom")), Generic},
		{"non-grpc context canceled", context.Canceled, Generic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FromError(tt.err); got != tt.want {
				t.Fatalf("FromError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}
