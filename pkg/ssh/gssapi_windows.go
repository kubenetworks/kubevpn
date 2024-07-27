//go:build windows

package ssh

func GetKrb5Path() string {
	return "C:\\ProgramData\\MIT\\Kerberos5\\krb5.ini"
}
