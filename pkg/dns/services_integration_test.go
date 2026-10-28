package dns

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Exercise generation, persistence, deduplication and logging together, without
// opening the system hosts file, its inter-process lock, or OS resolver files.
func serviceHostsEnv(t *testing.T) (*Config, string, context.Context, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &Config{Lock: &sync.Mutex{}, TunName: "test-services"}
	c.readHosts = func() ([]byte, error) { return os.ReadFile(path) }
	c.writeHosts = func(content string) error {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(content)
		return err
	}
	buf := &bytes.Buffer{}
	ctx := plog.WithLogger(context.Background(), plog.GetLoggerForServer(int32(logrus.DebugLevel), buf))
	return c, path, ctx, buf
}

func assertServiceHosts(t *testing.T, path string, domains ...string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range domains {
		count := 0
		for _, line := range strings.Split(string(content), "\n") {
			if lineHasDomain(line, domain) {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%s occurs in %d hosts lines: %s", domain, count, content)
		}
	}
}

func TestIntegration_UpdateServicesHostsLifecycle(t *testing.T) {
	c, path, ctx, logs := serviceHostsEnv(t)
	n, err := c.AddServiceNameToHosts(ctx, Entry{IP: "10.1.0.1", Domain: "extra"})
	if err != nil || n != 1 {
		t.Fatalf("initial hosts = %d, %v", n, err)
	}
	services := []corev1.Service{svc("web", "10.96.0.1"), svc("headless", "None"), svc("kubernetes", "10.96.0.2")}
	c.UpdateServices(ctx, services)
	assertServiceHosts(t, path, "localhost", "extra", "web")
	if !strings.Contains(logs.String(), "Added 1 hosts entries from service discovery") {
		t.Errorf("missing successful append count: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "3 services") {
		t.Errorf("missing input service count: %s", logs.String())
	}
	logs.Reset()
	c.UpdateServices(ctx, services)
	if strings.Contains(logs.String(), "info:") {
		t.Errorf("duplicate update reported new entries: %s", logs.String())
	}
	assertServiceHosts(t, path, "extra", "web")

	// A delta changes the known service set, but hosts remain add-only.
	logs.Reset()
	c.UpdateServices(ctx, []corev1.Service{svc("later", "10.96.0.3")})
	assertServiceHosts(t, path, "web", "later", "extra")
	if len(c.Services) != 1 || c.Services[0].Name != "later" {
		t.Fatalf("service set not replaced: %v", c.Services)
	}
	if !strings.Contains(logs.String(), "Added 1 hosts entries") {
		t.Errorf("delta append missing: %s", logs.String())
	}
	logs.Reset()
	c.UpdateServices(ctx, nil)
	assertServiceHosts(t, path, "web", "later", "extra")
	if strings.Contains(logs.String(), "info:") {
		t.Errorf("empty update reported new entries: %s", logs.String())
	}
}

func TestIntegration_UpdateServicesHostsFailureRecovery(t *testing.T) {
	for _, failure := range []string{"open", "canceled-write", "canceled-parent"} {
		t.Run(failure, func(t *testing.T) {
			c, path, ctx, logs := serviceHostsEnv(t)
			write := c.writeHosts
			services := []corev1.Service{svc("web", "10.96.0.1")}
			switch failure {
			case "open":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "canceled-write":
				c.writeHosts = func(string) error { return context.Canceled }
			case "canceled-parent":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				c.UpdateServices(canceled, services)
			}
			if failure != "canceled-parent" {
				c.UpdateServices(ctx, services)
			}
			if strings.Contains(logs.String(), "info:") {
				t.Errorf("failed/canceled update reported success: %s", logs.String())
			}
			if failure == "open" && !strings.Contains(logs.String(), "error:") {
				t.Errorf("missing write error: %s", logs.String())
			}
			// Recover with the real file writer; bookkeeping alone must not suppress a retry.
			if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0600); err != nil {
				t.Fatal(err)
			}
			c.writeHosts = write
			logs.Reset()
			c.UpdateServices(ctx, services)
			assertServiceHosts(t, path, "localhost", "web")
			if !strings.Contains(logs.String(), "Added 1 hosts entries") {
				t.Errorf("recovery success missing: %s", logs.String())
			}
		})
	}
}
