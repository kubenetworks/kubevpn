package wintun

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

// driver download from: https://www.wintun.net/builds/wintun-0.14.1.zip
func copyDriver(b []byte) error {
	executable, err := os.Executable()
	if err != nil {
		err = errors.Wrap(err, "Failed to get executable path ")
		return err
	}
	filename := filepath.Join(filepath.Dir(executable), "wintun.dll")
	var content []byte
	content, err = os.ReadFile(filename)
	if err == nil {
		// already exists and content are same, not need to copy this file
		if bytes.Compare(b, content) == 0 {
			return nil
		}
		_ = os.Remove(filename)
	}
	err = os.WriteFile(filename, b, 644)
	return err
}
