//go:build !windows

package ssh

// GetKrb5Path returns the default path to the Kerberos 5 configuration file on Unix systems.
func GetKrb5Path() string {
	return "/etc/krb5.conf"
}
