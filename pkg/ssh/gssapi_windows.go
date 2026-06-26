//go:build windows

package ssh

// GetKrb5Path returns the default path to the Kerberos 5 configuration file on Windows.
func GetKrb5Path() string {
	return "C:\\ProgramData\\MIT\\Kerberos5\\krb5.ini"
}
