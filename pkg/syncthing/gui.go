package syncthing

import (
	"archive/zip"
	"bytes"
	"embed"
	"io"
	"os"
	"path/filepath"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

//go:embed gui.zip
var assetsZip embed.FS

func MakeSureGui() error {
	return ExtractSyncthingGUIZipToDir(assetsZip, "gui.zip", config.GetSyncthingPath())
}

func ExtractSyncthingGUIZipToDir(fs embed.FS, zipPath, targetDir string) error {
	zipData, err := fs.Open(zipPath)
	if err != nil {
		return err
	}
	defer zipData.Close()

	all, err := io.ReadAll(zipData)
	if err != nil {
		return err
	}
	zipReader, err := zip.NewReader(bytes.NewReader(all), int64(len(all)))
	if err != nil {
		return err
	}

	for _, file := range zipReader.File {
		filePath := filepath.Join(targetDir, file.Name)

		if file.FileInfo().IsDir() {
			if err = os.MkdirAll(filePath, file.Mode()); err != nil {
				return err
			}
			continue
		}

		if err = os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return err
		}

		var fileWriter *os.File
		fileWriter, err = os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
		if err != nil {
			return err
		}

		var fileReader io.ReadCloser
		fileReader, err = file.Open()
		if err != nil {
			_ = fileWriter.Close()
			return err
		}

		_, err = io.Copy(fileWriter, fileReader)
		_ = fileReader.Close()
		_ = fileWriter.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
