package util

import (
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

func DownloadFileWithName(uri, name string) (string, error) {
	resp, err := getWithRetry(uri)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading file %s failed. status code: %d, expected: %d", uri, resp.StatusCode, http.StatusOK)
	}

	dir, err := os.MkdirTemp("", "")
	if err != nil {
		return "", err
	}

	file := filepath.Join(dir, name)
	out, err := os.Create(file)
	if err != nil {
		return "", err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to save file %s. error: %v", file, err)
	}

	log.Infof("Downloaded file %s", file)
	return file, nil
}

func DownloadFileStream(uri string) ([]byte, error) {
	resp, err := getWithRetry(uri)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading file %s failed. status code: %d, expected: %d", uri, resp.StatusCode, http.StatusOK)
	}
	return io.ReadAll(resp.Body)
}

func DownloadFile(uri string) (string, error) {
	return DownloadFileWithName(uri, fmt.Sprintf("%d", time.Now().Unix()))
}

const (
	maxRetries   = 4
	retryWaitMin = 2 * time.Second
	retryWaitMax = 10 * time.Second
)

func getWithRetry(uri string) (*http.Response, error) {
	var resp *http.Response
	var err error

	for i := 0; i < maxRetries; i++ {
		resp, err = http.Get(uri)
		shouldRetry := checkRetry(resp, err)
		if !shouldRetry {
			break
		}

		drainBody(resp.Body)
		wait := backoff(retryWaitMin, retryWaitMax, i)
		<-time.After(wait)
	}

	return resp, err
}

func checkRetry(resp *http.Response, err error) bool {
	return resp != nil && resp.StatusCode == http.StatusNotFound
}

func drainBody(b io.ReadCloser) {
	defer b.Close()
	io.Copy(io.Discard, io.LimitReader(b, int64(4096)))
}

func backoff(min, max time.Duration, attemptNum int) time.Duration {
	mult := math.Pow(2, float64(attemptNum)) * float64(min)
	sleep := time.Duration(mult)
	if float64(sleep) != mult || sleep > max {
		sleep = max
	}
	return sleep
}

func ParseDirMapping(dir string) (local, remote string, err error) {
	index := strings.LastIndex(dir, ":")
	if index < 0 {
		err = fmt.Errorf("directory mapping is invaild: %s", dir)
		return
	}
	local = dir[:index]
	_, err = os.Stat(local)
	if err != nil {
		return
	}
	remote = dir[index+1:]
	return
}

func CleanupTempKubeConfigFile() error {
	return filepath.Walk(os.TempDir(), func(path string, info fs.FileInfo, err error) error {
		if strings.HasSuffix(path, ".kubeconfig") {
			return os.Remove(path)
		}
		return err
	})
}
