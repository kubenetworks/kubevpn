package util

import (
	"fmt"
	"strings"
)

func Join(names ...string) string {
	return strings.Join(names, "_")
}

func ContainerNet(name string) string {
	return fmt.Sprintf("container:%s", name)
}

func GenEnvoyUID(ns, uid string) string {
	return fmt.Sprintf("%s.%s", ns, uid)
}
