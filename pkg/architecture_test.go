package pkg_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestArchitectureDependencies checks direct production imports, including files
// for other operating systems. It deliberately does not use go list/build tags:
// a forbidden dependency must not hide behind a platform or integration tag.
func TestArchitectureDependencies(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Go runs this package's tests in pkg/, also when built with -trimpath.
	module, err := os.ReadFile(filepath.Join(root, "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var modulePath string
	for _, line := range strings.Split(string(module), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			modulePath = strings.Trim(fields[1], "\"")
			break
		}
	}
	if modulePath == "" {
		t.Fatal("go.mod has no module directive")
	}

	violations, err := checkArchitectureDependencies(root, modulePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Error(violation)
	}
}

// root is the pkg directory, real or a temporary source tree. Returning
// diagnostics lets fixtures exercise the same parser and rules as the repo.
func checkArchitectureDependencies(root, modulePath string) ([]string, error) {
	type boundary struct {
		from string
		deny []string
	}
	// Only established leaf boundaries are enforced at stage 0. In particular,
	// daemon/rpc is a wire contract, NOT daemon/action. Existing leaves may
	// import it, but the future tunnel package must remain RPC-independent.
	var rules []boundary
	for _, leaf := range []string{"config", "log", "driver", "dhcp", "tun", "ssh", "dns", "inject", "controlplane"} {
		rules = append(rules, boundary{
			from: "pkg/" + leaf,
			deny: []string{"pkg/handler", "pkg/core", "pkg/network", "pkg/tunnel", "pkg/daemon/action"},
		})
	}
	// These rules activate naturally when the source packages are introduced.
	// Legacy callers are intentionally unrestricted until their migration stage.
	rules = append(rules,
		boundary{"pkg/cluster", []string{"pkg/handler", "pkg/core", "pkg/network", "pkg/tunnel", "pkg/xds", "pkg/daemon/action"}},
		boundary{"pkg/network", []string{"pkg/handler", "pkg/xds", "pkg/daemon/action"}},
		boundary{"pkg/tunnel", []string{"pkg/handler", "pkg/core", "pkg/network", "pkg/cluster", "pkg/xds", "pkg/inject", "pkg/controlplane", "pkg/daemon/action", "pkg/daemon/rpc", "k8s.io", "sigs.k8s.io", "github.com/spf13/cobra", "google.golang.org/grpc"}},
		boundary{"pkg/xds/internal", []string{"pkg/handler", "pkg/core", "pkg/network", "pkg/tunnel", "pkg/daemon/action", "pkg/daemon/rpc", "google.golang.org/grpc"}},
	)
	within := func(path, parent string) bool {
		return path == parent || strings.HasPrefix(path, parent+"/")
	}
	var violations []string
	files := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == "vendor" || entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		source := "pkg/" + filepath.ToSlash(filepath.Dir(rel))
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		files++
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			// Keep external import paths intact so transport/framework rules
			// are checked too, rather than skipping non-module imports.
			target := strings.TrimPrefix(importPath, modulePath+"/")
			reject := func(reason string) {
				violations = append(violations, fmt.Sprintf("%s:%d: %s imports %s: %s", filepath.ToSlash(rel), fset.Position(spec.Pos()).Line, source, target, reason))
			}
			for _, rule := range rules {
				if !within(source, rule.from) {
					continue
				}
				for _, denied := range rule.deny {
					if within(target, denied) {
						reject("reverse dependency across " + rule.from + " boundary")
					}
				}
			}
			// Only the legacy aggregate is forbidden; standalone algorithms
			// such as util/netutil remain available during extraction.
			if within(source, "pkg/cluster") && target == "pkg/util" {
				reject("cluster must not depend on the legacy util aggregate")
			}
			if within(source, "pkg/handler/internal/syncworkload") && target == "pkg/handler" {
				reject("sync runtime must not import its parent session adapter")
			}
			if within(source, "pkg/tunnel/internal/netstack") && target == "pkg/tunnel" {
				reject("netstack must not import its parent tunnel package")
			}
			if within(source, "pkg/internal/wire") && (within(target, "pkg/tunnel") || within(target, "pkg/core")) {
				reject("wire framing must not import its consumers")
			}
			if within(source, "pkg/xds/internal") && within(target, "pkg/xds") && !within(target, "pkg/xds/internal") {
				reject("xds internals must not depend on the public composition layer")
			}
			if !within(source, "pkg/xds") && within(target, "pkg/xds/internal") {
				reject("xds internals are private to xds")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if files == 0 {
		return nil, fmt.Errorf("architecture check did not inspect any production Go files")
	}
	return violations, nil
}

func TestArchitectureDependencyFixtures(t *testing.T) {
	const modulePath = "example.invalid/kubevpn"
	for _, tc := range []struct {
		name       string
		file       string
		importPath string
		wantReject bool
	}{
		{"leaf_action", "tun/source.go", modulePath + "/pkg/daemon/action", true},
		{"leaf_core", "dns/source.go", modulePath + "/pkg/core", true},
		{"leaf_rpc_allowed", "inject/source.go", modulePath + "/pkg/daemon/rpc", false},
		{"network_rpc_allowed", "network/source.go", modulePath + "/pkg/daemon/rpc", false},
		{"rpc_prefix_not_action", "ssh/source.go", modulePath + "/pkg/daemon/actionhelper", false},
		{"tunnel_rpc", "tunnel/source.go", modulePath + "/pkg/daemon/rpc", true},
		{"tunnel_kubernetes_windows", "tunnel/source_windows.go", "k8s.io/client-go/kubernetes", true},
		{"tunnel_kubernetes_linux", "tunnel/source_linux.go", "sigs.k8s.io/controller-runtime", true},
		{"tunnel_cobra", "tunnel/source.go", "github.com/spf13/cobra", true},
		{"tunnel_grpc", "tunnel/source.go", "google.golang.org/grpc", true},
		{"tunnel_grpc_subpackage", "tunnel/source.go", "google.golang.org/grpc/status", true},
		{"tunnel_stdlib", "tunnel/source.go", "net", false},
		{"netstack_parent", "tunnel/internal/netstack/source.go", modulePath + "/pkg/tunnel", true},
		{"wire_tunnel", "internal/wire/source.go", modulePath + "/pkg/tunnel", true},
		{"wire_core", "internal/wire/source.go", modulePath + "/pkg/core", true},
		{"cluster_util_aggregate", "cluster/internal/source.go", modulePath + "/pkg/util", true},
		{"cluster_util_leaf", "cluster/source.go", modulePath + "/pkg/util/netutil", false},
		{"cluster_kubernetes_allowed", "cluster/source.go", "k8s.io/client-go/kubernetes", false},
		{"network_handler", "network/source.go", modulePath + "/pkg/handler", true},
		{"xds_reverse", "xds/internal/source.go", modulePath + "/pkg/xds", true},
		{"xds_domain_rpc", "xds/internal/leases/source.go", modulePath + "/pkg/daemon/rpc", true},
		{"xds_domain_grpc", "xds/internal/discovery/source.go", "google.golang.org/grpc/status", true},
		{"sync_runtime_parent", "handler/internal/syncworkload/source.go", modulePath + "/pkg/handler", true},
		{"xds_private", "handler/source.go", modulePath + "/pkg/xds/internal/routes", true},
		{"xds_composition_allowed", "xds/source.go", modulePath + "/pkg/xds/internal/routes", false},
		{"test_ignored", "tunnel/source_test.go", "google.golang.org/grpc", false},
		{"gofixture_ignored", "tunnel/source_test.gofixture", "google.golang.org/grpc", false},
		{"vendor_ignored", "tunnel/vendor/source.go", "google.golang.org/grpc", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			// Keep one real production file so ignored-only fixtures and
			// not-yet-created migration packages exercise non-vacuous scans.
			if err := os.WriteFile(filepath.Join(root, "anchor.go"), []byte("package fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, filepath.FromSlash(tc.file))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			source := fmt.Sprintf("package fixture\nimport _ %q\n", tc.importPath)
			if strings.HasSuffix(tc.file, "_windows.go") {
				source = "//go:build windows\n\n" + source
			}
			if strings.HasSuffix(tc.file, "_linux.go") {
				source = "//go:build linux\n\n" + source
			}
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			violations, err := checkArchitectureDependencies(root, modulePath)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantReject {
				if len(violations) != 0 {
					t.Fatalf("allowed/ignored import rejected: %v", violations)
				}
				return
			}
			if len(violations) != 1 {
				t.Fatalf("want exactly one forbidden import, got %v", violations)
			}
			if !strings.Contains(violations[0], tc.file+":") ||
				!strings.Contains(violations[0], "imports "+strings.TrimPrefix(tc.importPath, modulePath+"/")+":") {
				t.Fatalf("diagnostic must identify source and forbidden import: %s", violations[0])
			}
		})
	}
}
