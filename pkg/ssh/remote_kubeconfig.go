package ssh

import (
	"bytes"
	"fmt"
	"path"

	"k8s.io/client-go/tools/clientcmd"
)

// embedRemoteCertFiles fetches any path-referenced cert/key/CA files from the
// remote host (over the existing SSH session) and inlines their contents into
// the kubeconfig, producing a self-contained byte blob. It is a no-op for
// configs that already carry inline *-data (paths empty).
//
// This exists because a kubeconfig fetched via --remote-kubeconfig may come from
// the `cat` fallback (when the bastion has neither kubectl nor minikube), which
// returns the RAW file. If that file references certificate/key files by
// relative path, the later local api.FlattenConfig (in util.ModifyAPIServer)
// would try to resolve those paths against the daemon CWD — the files live on
// the remote host, so the read fails with a cryptic "open ..: operation not
// permitted". By inlining here, over the same SSH session that fetched the
// config, FlattenConfig becomes a no-op instead.
//
//   remoteBaseDir - path.Dir(conf.RemoteKubeconfig) (POSIX); relative cert paths
//                   are resolved against it (matching how kubectl would resolve
//                   them on the remote, where the kubeconfig file is the base).
//   remoteRead    - fetches a resolved remote path's bytes. The caller wires it
//                   to RemoteRun(cli, shellquote.Join("cat", path), nil); tests
//                   inject a map-backed implementation.
func embedRemoteCertFiles(kubeconfigBytes []byte, remoteBaseDir string, remoteRead func(remotePath string) ([]byte, error)) ([]byte, error) {
	cfg, err := clientcmd.Load(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse remote kubeconfig: %w", err)
	}
	if cfg == nil {
		return kubeconfigBytes, nil
	}

	for name, authInfo := range cfg.AuthInfos {
		if authInfo == nil {
			continue
		}
		if len(authInfo.ClientCertificateData) == 0 && len(authInfo.ClientCertificate) != 0 {
			data, fetchErr := fetchRemoteCertFile(remoteBaseDir, authInfo.ClientCertificate, "client-certificate", remoteRead)
			if fetchErr != nil {
				return nil, fetchErr
			}
			authInfo.ClientCertificateData = bytes.TrimSpace(data)
			authInfo.ClientCertificate = ""
		}
		if len(authInfo.ClientKeyData) == 0 && len(authInfo.ClientKey) != 0 {
			data, fetchErr := fetchRemoteCertFile(remoteBaseDir, authInfo.ClientKey, "client-key", remoteRead)
			if fetchErr != nil {
				return nil, fetchErr
			}
			authInfo.ClientKeyData = bytes.TrimSpace(data)
			authInfo.ClientKey = ""
		}
		cfg.AuthInfos[name] = authInfo
	}

	for name, cluster := range cfg.Clusters {
		if cluster == nil {
			continue
		}
		if len(cluster.CertificateAuthorityData) == 0 && len(cluster.CertificateAuthority) != 0 {
			data, fetchErr := fetchRemoteCertFile(remoteBaseDir, cluster.CertificateAuthority, "certificate-authority", remoteRead)
			if fetchErr != nil {
				return nil, fetchErr
			}
			cluster.CertificateAuthorityData = bytes.TrimSpace(data)
			cluster.CertificateAuthority = ""
		}
		cfg.Clusters[name] = cluster
	}

	return clientcmd.Write(*cfg)
}

// fetchRemoteCertFile resolves a single cert/key/CA path against the remote
// kubeconfig directory and reads it via remoteRead, returning a clear, actionable
// error on failure instead of letting the caller surface an opaque OS error.
func fetchRemoteCertFile(remoteBaseDir, path, field string, remoteRead func(remotePath string) ([]byte, error)) ([]byte, error) {
	resolved := resolveRemotePath(remoteBaseDir, path)
	data, err := remoteRead(resolved)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch remote %s %q (resolved to %q) referenced by --remote-kubeconfig: %w; ensure the path is readable on the bastion, or use a self-contained kubeconfig (embed cert/key data inline)", field, path, resolved, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("remote %s %q (resolved to %q) is empty; ensure the path is readable on the bastion, or use a self-contained kubeconfig (embed cert/key data inline)", field, path, resolved)
	}
	return data, nil
}

// resolveRemotePath resolves a cert/key/CA path the way kubectl would on the
// remote host: absolute paths are taken verbatim, relative paths are resolved
// against the directory of the remote kubeconfig file. Uses POSIX semantics
// (path, not path/filepath) because the paths live on a remote Linux bastion
// regardless of the local OS running kubevpn — otherwise Windows clients would
// mangle "/etc/..." into "\etc\..." and fail to fetch the files.
func resolveRemotePath(remoteBaseDir, p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Clean(path.Join(remoteBaseDir, p))
}
