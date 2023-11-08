package cp

import (
	"archive/tar"
	"io"
	"os"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

// copy from another real file
func copyFromLink(fileHeaderList []tar.Header, currFile tar.Header, genDstFilename func(headerName string) localPath) error {
	for _, t := range fileHeaderList {
		if t.Name == currFile.Linkname {
			// handle it recursive if linkA --> linkB --> originFile
			return copyFromLink(fileHeaderList, t, genDstFilename)
		}
	}

	var err error
	var r, w *os.File
	// read from origin file
	r, err = os.OpenFile(genDstFilename(currFile.Linkname).String(), os.O_RDONLY, 0644)
	if err != nil {
		err = errors.Wrap(err, "os.OpenFile(genDstFilename(currFile.Linkname).String(), os.O_RDONLY, 0644): ")
		return err
	}

	// write to current file
	w, err = os.OpenFile(genDstFilename(currFile.Name).String(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		err = errors.Wrap(err, "os.OpenFile(genDstFilename(currFile.Name).String(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644): ")
		return err
	}
	_, err = io.Copy(w, r)
	if closeErr := w.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
}
