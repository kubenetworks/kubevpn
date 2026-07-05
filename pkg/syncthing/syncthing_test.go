package syncthing

import (
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/tlsutil"
	"sigs.k8s.io/yaml"
)

func TestGenerateCertificate(t *testing.T) {
	dir := t.TempDir()
	cert, err := tlsutil.NewCertificate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), "syncthing", 365000)
	if err != nil {
		t.Fatal(err)
	}
	marshal, err := yaml.Marshal(cert)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(marshal))
	id := protocol.NewDeviceID(cert.Certificate[0])
	t.Log(id)
}
