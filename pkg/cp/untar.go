package cp

import (
	"archive/tar"
	"fmt"
	"io"
	"os"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// copy from another real file
func copyFromLink(fileHeaderList []tar.Header, currFile tar.Header, genDstFilename func(headerName string) localPath) error {
	return copyFromLinkVisited(fileHeaderList, currFile, genDstFilename, make(map[string]bool))
}

func copyFromLinkVisited(fileHeaderList []tar.Header, currFile tar.Header, genDstFilename func(headerName string) localPath, visited map[string]bool) error {
	// Guard against circular link chains (e.g. A -> B -> A) that would otherwise
	// recurse until the stack overflows.
	if visited[currFile.Name] {
		return fmt.Errorf("circular link chain detected at %q", currFile.Name)
	}
	visited[currFile.Name] = true

	for _, t := range fileHeaderList {
		if t.Name == currFile.Linkname {
			// handle it recursive if linkA --> linkB --> originFile
			return copyFromLinkVisited(fileHeaderList, t, genDstFilename, visited)
		}
	}

	var err error
	var r, w *os.File
	// read from origin file
	r, err = os.OpenFile(genDstFilename(currFile.Linkname).String(), os.O_RDONLY, config.FileModeFile)
	if err != nil {
		return err
	}
	defer r.Close()

	// write to current file
	w, err = os.OpenFile(genDstFilename(currFile.Name).String(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, config.FileModeFile)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	if closeErr := w.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
}
