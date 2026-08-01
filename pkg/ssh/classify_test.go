package ssh

import (
	"errors"
	"fmt"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestWrapDialError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"auth", errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey]"), config.ErrSSHAuth},
		{"gssapi", errors.New("error generating new kerberos 5 token"), config.ErrGSSAPI},
		{"connect refused", errors.New("dial tcp 1.2.3.4:22: connect: connection refused"), config.ErrSSHConnect},
		{"nil", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapDialError(tt.err)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("wrapDialError(nil) = %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, tt.want) {
				t.Fatalf("wrapDialError(%v) does not wrap %v", tt.err, tt.want)
			}
			// Classification must survive an outer "failed to jump: %w" wrap.
			outer := fmt.Errorf("failed to jump to host: %w", got)
			if !errors.Is(outer, tt.want) {
				t.Fatalf("classification lost through outer wrap for %q", tt.name)
			}
		})
	}
}
