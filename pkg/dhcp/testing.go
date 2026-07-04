package dhcp

// NewManagerForTest creates a Manager with the given connectionID for unit testing.
// This avoids needing a real Kubernetes cluster to test connection identity.
func NewManagerForTest(connectionID string) *Manager {
	return &Manager{
		connectionID: connectionID,
	}
}
