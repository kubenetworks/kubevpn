package tun

// ChangeIP replaces the IP address on an existing TUN device without destroying it.
// The TUN file descriptor remains valid — only the OS interface metadata changes.
// oldAddr can be empty to skip deletion (only add new).
// Both addresses must be in CIDR notation (e.g. "198.18.0.5/32").
func ChangeIP(ifName string, oldAddr, newAddr string) error {
	return changeIP(ifName, oldAddr, newAddr)
}
