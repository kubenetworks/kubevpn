package cp

import (
	"archive/tar"
	"io"
	"os"
)

func process(header []tar.Header, f tar.Header, dir func(headerName string) localPath) error {
	for _, t := range header {
		if t.Name == f.Linkname {
			return process(header, t, dir)
		}
	}
	abs := dir(f.Name)
	linked := dir(f.Linkname)

	wf, err := os.OpenFile(abs.String(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		panic(err)
	}
	read, err := os.OpenFile(linked.String(), os.O_RDONLY, 0644)
	if err != nil {
		panic(err)
	}
	_, err = io.Copy(wf, read)
	if closeErr := wf.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
}
