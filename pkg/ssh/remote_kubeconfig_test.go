package ssh

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// rawKubeconfigBytes builds a kubeconfig that references cert/key/CA files by
// path (not inline data), mirroring what the `cat` fallback returns when the
// bastion has no kubectl.
func rawKubeconfigBytes(t *testing.T, ca, cert, key string) []byte {
	t.Helper()
	cfg := api.NewConfig()
	cfg.Clusters["default"] = &api.Cluster{
		Server:                 "https://127.0.0.1:6443",
		CertificateAuthority:   ca,
		LocationOfOrigin:       "", // bytes-loaded: no origin, like NewClientConfigFromBytes
	}
	cfg.AuthInfos["default"] = &api.AuthInfo{
		ClientCertificate: cert,
		ClientKey:         key,
		LocationOfOrigin:  "",
	}
	cfg.Contexts["default"] = &api.Context{Cluster: "default", AuthInfo: "default"}
	cfg.CurrentContext = "default"
	out, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("clientcmd.Write: %v", err)
	}
	return out
}

// selfContainedKubeconfigBytes builds a kubeconfig that already carries inline
// cert/key/CA data (paths empty), as kubectl --flatten would produce.
func selfContainedKubeconfigBytes(t *testing.T) []byte {
	t.Helper()
	cfg := api.NewConfig()
	cfg.Clusters["default"] = &api.Cluster{
		Server:                   "https://127.0.0.1:6443",
		CertificateAuthorityData: []byte("CA-DATA"),
	}
	cfg.AuthInfos["default"] = &api.AuthInfo{
		ClientCertificateData: []byte("CERT-DATA"),
		ClientKeyData:         []byte("KEY-DATA"),
	}
	cfg.Contexts["default"] = &api.Context{Cluster: "default", AuthInfo: "default"}
	cfg.CurrentContext = "default"
	out, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("clientcmd.Write: %v", err)
	}
	return out
}

func parseConfig(t *testing.T, b []byte) *api.Config {
	t.Helper()
	cfg, err := clientcmd.Load(b)
	if err != nil {
		t.Fatalf("clientcmd.Load: %v", err)
	}
	return cfg
}

// TestEmbedRemoteCertFiles_RelativePathsInlined verifies that relative cert/key/CA
// path references are resolved against the remote kubeconfig directory, fetched
// over the SSH callback, and inlined — turning the raw config self-contained.
func TestEmbedRemoteCertFiles_RelativePathsInlined(t *testing.T) {
	in := rawKubeconfigBytes(t, "..", "client.crt", "client.key")
	remoteBaseDir := "/etc/rancher/k3s"

	// Expected resolved paths: ".." → /etc/rancher; relative names joined to base.
	files := map[string][]byte{
		"/etc/rancher":           []byte("ca-pem"),
		"/etc/rancher/k3s/client.crt": []byte("cert-pem"),
		"/etc/rancher/k3s/client.key": []byte("key-pem"),
	}
	calls := 0
	remoteRead := func(p string) ([]byte, error) {
		calls++
		b, ok := files[p]
		if !ok {
			t.Errorf("unexpected remoteRead path %q", p)
			return nil, errors.New("not found")
		}
		return b, nil
	}

	out, err := embedRemoteCertFiles(in, remoteBaseDir, remoteRead)
	if err != nil {
		t.Fatalf("embedRemoteCertFiles: %v", err)
	}
	if calls != 3 {
		t.Fatalf("remoteRead called %d times, want 3", calls)
	}

	cfg := parseConfig(t, out)
	if got := cfg.Clusters["default"].CertificateAuthority; got != "" {
		t.Errorf("CertificateAuthority path not cleared: %q", got)
	}
	if got := string(cfg.Clusters["default"].CertificateAuthorityData); got != "ca-pem" {
		t.Errorf("CertificateAuthorityData = %q, want ca-pem", got)
	}
	auth := cfg.AuthInfos["default"]
	if auth.ClientCertificate != "" {
		t.Errorf("ClientCertificate path not cleared: %q", auth.ClientCertificate)
	}
	if got := string(auth.ClientCertificateData); got != "cert-pem" {
		t.Errorf("ClientCertificateData = %q, want cert-pem", got)
	}
	if auth.ClientKey != "" {
		t.Errorf("ClientKey path not cleared: %q", auth.ClientKey)
	}
	if got := string(auth.ClientKeyData); got != "key-pem" {
		t.Errorf("ClientKeyData = %q, want key-pem", got)
	}
}

// TestEmbedRemoteCertFiles_AlreadySelfContainedNoop verifies the helper does not
// touch the SSH session when the config already carries inline data.
func TestEmbedRemoteCertFiles_AlreadySelfContainedNoop(t *testing.T) {
	in := selfContainedKubeconfigBytes(t)
	remoteRead := func(p string) ([]byte, error) {
		t.Errorf("remoteRead must not be called for a self-contained config (path %q)", p)
		return nil, nil
	}

	out, err := embedRemoteCertFiles(in, "/etc/rancher/k3s", remoteRead)
	if err != nil {
		t.Fatalf("embedRemoteCertFiles: %v", err)
	}
	cfg := parseConfig(t, out)
	if got := string(cfg.Clusters["default"].CertificateAuthorityData); got != "CA-DATA" {
		t.Errorf("CertificateAuthorityData changed to %q", got)
	}
	if got := string(cfg.AuthInfos["default"].ClientKeyData); got != "KEY-DATA" {
		t.Errorf("ClientKeyData changed to %q", got)
	}
}

// TestEmbedRemoteCertFiles_UnreadablePathClearError verifies a remote read
// failure yields an actionable error naming the field and path, with the
// underlying error chained for errors.Is.
func TestEmbedRemoteCertFiles_UnreadablePathClearError(t *testing.T) {
	in := rawKubeconfigBytes(t, "..", "client.crt", "client.key")
	sentinel := errors.New("cat: ..: Is a directory")
	remoteRead := func(p string) ([]byte, error) { return nil, sentinel }

	_, err := embedRemoteCertFiles(in, "/etc/rancher/k3s", remoteRead)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error does not chain underlying error: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"client-certificate", "self-contained kubeconfig"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// TestEmbedRemoteCertFiles_AbsoluteRemotePath verifies absolute cert paths are
// fetched verbatim, ignoring remoteBaseDir (matching how kubectl resolves them).
func TestEmbedRemoteCertFiles_AbsoluteRemotePath(t *testing.T) {
	const abs = "/var/lib/rancher/k3s/server/tls/client-ca.crt"
	in := rawKubeconfigBytes(t, abs, "/etc/client.crt", "/etc/client.key")
	files := map[string][]byte{
		abs:             []byte("abs-ca"),
		"/etc/client.crt": []byte("abs-cert"),
		"/etc/client.key": []byte("abs-key"),
	}
	var seenCA string
	remoteRead := func(p string) ([]byte, error) {
		if p == abs {
			seenCA = p
		}
		b, ok := files[p]
		if !ok {
			t.Errorf("unexpected remoteRead path %q", p)
			return nil, errors.New("not found")
		}
		return b, nil
	}

	out, err := embedRemoteCertFiles(in, "/this/base/is/ignored", remoteRead)
	if err != nil {
		t.Fatalf("embedRemoteCertFiles: %v", err)
	}
	if seenCA != abs {
		t.Errorf("CA fetched as %q, want absolute %q", seenCA, abs)
	}
	cfg := parseConfig(t, out)
	if got := string(cfg.Clusters["default"].CertificateAuthorityData); got != "abs-ca" {
		t.Errorf("CertificateAuthorityData = %q, want abs-ca", got)
	}
}
