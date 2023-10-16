package main

// this is a very simplified version of https://github.com/hashicorp/go-retryablehttp
// reason we pulled in here is because we use gock for testing this tool, and gock does
// not work very well with go-retryablehttp as gock replaces transport required by go-retryablehttp

import (
	"io"
	"math"
	"net/http"
	"time"
)

const (
	maxRetries   = 4
	retryWaitMin = 2 * time.Second
	retryWaitMax = 10 * time.Second
)

// getWithRetry is basically http.Get with retries
// we cannot use RoundTripper as gock (lib we use for testing)
// overrides the Transport and thus we cannot test our retryable transport
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
