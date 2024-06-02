package incontainer

import (
	"bytes"
	"os"
	"syscall"
)

func Detect() bool {
	return hasContainerEnv() || isCgroupV2Container() || hasDockerCgroup() || pid1EnvHasContainer() || syscall.Getpid() == 1 || isOpenVZ() || isWSL() || isKubernetes()
}

func hasContainerEnv() bool {
	return exists("/.dockerenv") || exists("/run/.containerenv")
}

func isOpenVZ() bool {
	return exists("/proc/vz") && !exists("/proc/bc")
}

func exists(file string) bool {
	_, err := os.Stat(file)
	return err == nil
}

func hasDockerCgroup() bool {
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return false
	}
	return bytes.Contains(cgroup, []byte("/docker/"))
}

func isCgroupV2Container() bool {
	if !exists("/sys/fs/cgroup/cgroup.events") {
		return false
	}
	if exists("/sys/fs/cgroup/cgroup.type") {
		return true
	}
	if !exists("/sys/kernel/cgroup/features") {
		return true
	}
	return false
}

func pid1EnvHasContainer() bool {
	bs, err := os.ReadFile("/proc/1/environ")
	if err != nil {
		return false
	}
	return bytes.Contains(bs, []byte("CONTAINER="))
}

func isWSL() bool {
	bs, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return false
	}
	return bytes.Contains(bs, []byte("Microsoft"))
}

func isKubernetes() bool {
	return os.Getenv("KUBERNETES_SERVICE_HOST") != ""
}
