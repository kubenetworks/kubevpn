//go:build windows

package action

// sudoProcessAliveImpl is a no-op on Windows, where there is no cheap signal-0 liveness check:
// it returns true so MonitorSudoLiveness falls through to the gRPC probe, which still detects a
// down daemon via dial failure. The crash→"disconnected" refinement the PID pre-check provides
// on Unix degrades to "unhealthy" here, which is acceptable.
func sudoProcessAliveImpl() bool {
	return true
}
