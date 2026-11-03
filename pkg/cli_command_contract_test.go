package pkg_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/cmd/kubevpn/cmds"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util/exitcode"
)

// Execute the frozen command tree inside the test process. Help and flag
// validation must finish without starting daemons or contacting Kubernetes.
func TestCLICommandContract(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, "missing-kubeconfig"))
	image, debug := config.Image, config.Debug
	t.Cleanup(func() { config.Image, config.Debug = image, debug })

	type result struct {
		Args   []string
		Output string
		Error  string
		Code   int
	}
	var results []result
	for _, args := range [][]string{
		{"--help"},
		{"connect", "--help"},
		{"proxy", "--help"},
		{"sync", "--help"},
		{"run", "--help"},
		{"status", "--help"},
		{"version", "--help"},
		{"server", "--help"},
		{"server", "--definitely-invalid-flag"},
		{"--definitely-invalid-flag"},
		{"connect", "--definitely-invalid-flag"},
	} {
		cmd := cmds.NewKubeVPNCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := cmd.ExecuteContext(ctx)
		cancel()
		r := result{Args: args, Output: strings.ReplaceAll(out.String(), home, "<HOME>"), Code: exitcode.FromError(err)}
		invalid := args[len(args)-1] == "--definitely-invalid-flag"
		if err != nil {
			r.Error = err.Error()
		}
		if invalid {
			if err == nil || !strings.Contains(r.Error, "unknown flag") || r.Code == 0 {
				t.Fatalf("invalid flag %v: code=%d error=%v", args, r.Code, err)
			}
		} else if err != nil || r.Code != 0 || !strings.Contains(r.Output, "Usage:") {
			t.Fatalf("help %v: code=%d error=%v output=%q", args, r.Code, err, r.Output)
		}
		results = append(results, r)
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	// The same test is run against the preserved pre-refactor source tree; the
	// record permits an exact before/after comparison outside the repository.
	t.Logf("CLI_CONTRACT=%s", encoded)
}
