package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// ClientIDFile is the file under the kubevpn home directory that stores the
// stable client identifier.
const ClientIDFile = "client_id"

var (
	clientIDOnce sync.Once
	clientID     string
)

// GetClientID returns a stable per-(machine, OS user) client identifier, used as
// the TUN IP lease OwnerID so a client reconnects to the same IP. It is the
// first 12 characters of a UUID persisted at ~/.kubevpn/client_id, generated on
// first use and reused thereafter (and across daemon restarts).
//
// If persistence fails (e.g. read-only home), a fresh random ID is returned,
// degrading gracefully to the previous random-per-connect behavior.
func GetClientID() string {
	clientIDOnce.Do(func() {
		path := filepath.Join(homePath, ClientIDFile)
		if data, err := os.ReadFile(path); err == nil {
			if s := strings.TrimSpace(string(data)); len(s) >= 12 {
				clientID = s[:12]
				return
			}
		}
		id := uuid.New().String()
		// Best-effort persist; on failure still return a usable (non-persisted) ID.
		_ = os.WriteFile(path, []byte(id), FileModeFile)
		clientID = id[:12]
	})
	return clientID
}
