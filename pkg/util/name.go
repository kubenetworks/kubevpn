package util

import "strings"

func Join(names ...string) string {
	return strings.Join(names, "_")
}
