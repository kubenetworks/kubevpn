package dev

import "os"

func isRuntimeSig(_ os.Signal) bool {
	return false
}
