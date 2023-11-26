//go:build !windows

package util

func GetKrb5Path() string {
	return "/etc/krb5.conf"
}
