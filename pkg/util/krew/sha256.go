package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

const retries = 4

// DownloadFileWithName downloads a file with name
func DownloadFileWithName(uri, name string) (string, error) {
	resp, err := getWithRetry(uri)
	if err != nil {
		err = errors.Wrap(err, "getWithRetry(uri): ")
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errors.Errorf("downloading file %s failed. status code: %d, expected: %d", uri, resp.StatusCode, http.StatusOK)
	}

	dir, err := os.MkdirTemp("", "")
	if err != nil {
		err = errors.Wrap(err, "os.MkdirTemp(\"\", \"\"): ")
		return "", err
	}

	file := filepath.Join(dir, name)
	out, err := os.Create(file)
	if err != nil {
		err = errors.Wrap(err, "os.Create(file): ")
		return "", err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return "", errors.Errorf("failed to save file %s. error: %v", file, err)
	}

	logrus.Infof("downloaded file %s", file)
	return file, nil
}

func downloadFile(uri string) (string, error) {
	return DownloadFileWithName(uri, fmt.Sprintf("%d", time.Now().Unix()))
}

func GetSha256ForAsset(uri string) (string, error) {
	file, err := downloadFile(uri)
	if err != nil {
		err = errors.Wrap(err, "downloadFile(uri): ")
		return "", err
	}

	defer os.Remove(file)
	sha256, err := getSha256(file)
	if err != nil {
		err = errors.Wrap(err, "getSha256(file): ")
		return "", err
	}

	return sha256, nil
}

func getSha256(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		err = errors.Wrap(err, "os.Open(filename): ")
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
