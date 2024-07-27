//go:build !windows

package ssh

func GetKrb5Path() string {
	return "/etc/krb5.conf"
}
