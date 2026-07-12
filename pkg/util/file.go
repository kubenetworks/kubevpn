package util

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// DownloadFileWithName downloads the file at uri into a temp directory with the given filename and returns the path.
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
		return "", fmt.Errorf("failed to save file %s: %w", file, err)
	}

	return file, nil
}

// DownloadFileStream downloads the file at uri and returns its contents as a byte slice.
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

// drainBodyLimit caps how many bytes are drained from a response body before close, to allow connection reuse.
const drainBodyLimit = 4096

func drainBody(b io.ReadCloser) {
	defer b.Close()
	io.Copy(io.Discard, io.LimitReader(b, drainBodyLimit))
}

func backoff(min, max time.Duration, attemptNum int) time.Duration {
	mult := math.Pow(2, float64(attemptNum)) * float64(min)
	sleep := time.Duration(mult)
	if float64(sleep) != mult || sleep > max {
		sleep = max
	}
	return sleep
}

// ParseDirMapping splits a "local:remote" directory mapping string.
func ParseDirMapping(dir string) (local, remote string, err error) {
	index := strings.LastIndex(dir, ":")
	if index < 0 {
		err = fmt.Errorf("directory mapping is invalid: %s: %w", dir, config.ErrInvalidArgument)
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

// CleanupTempKubeConfigFile removes all temporary kubeconfig files from the KubeVPN temp directory.
func CleanupTempKubeConfigFile() error {
	return cleanupTempFilesIn(config.GetTempPath())
}

// cleanupTempFilesIn removes every regular file under dir (keeping directories).
// A missing dir is not an error — there is simply nothing to clean.
func cleanupTempFilesIn(dir string) error {
	return filepath.WalkDir(dir, func(path string, info fs.DirEntry, err error) error {
		// WalkDir passes a non-nil err (and a nil info) when it cannot stat an
		// entry — most notably the root dir itself when it does not exist.
		// Dereferencing info.IsDir() here would panic, so handle err first.
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		return os.Remove(path)
	})
}
