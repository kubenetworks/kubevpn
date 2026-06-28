package util

import (
	"bufio"
	"bytes"
	"os/exec"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// RunWithRollingOutWithChecker runs a command, streaming its combined output line by line
// to the checker function. When checker returns true, output streaming stops (but the
// command keeps running). Returns stdout, stderr, and any error after the command exits.
func RunWithRollingOutWithChecker(cmd *exec.Cmd, checker func(log string) bool) (string, string, error) {
	var stdout, stderr bytes.Buffer

	pr, pw, err := pipeWithBuffer()
	if err != nil {
		return "", "", err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		return "", "", err
	}

	stopped := false
	go func() {
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			line := scanner.Text()
			stdout.WriteString(line + "\n")
			if !stopped && checker(line) {
				stopped = true
			}
		}
	}()

	err = cmd.Wait()
	pw.Close()
	return stdout.String(), stderr.String(), err
}

func pipeWithBuffer() (*bufio.Reader, *pipeWriter, error) {
	pr, pw := newPipe()
	return bufio.NewReaderSize(pr, config.LargeBufferSize), pw, nil
}

type pipeReader struct{ ch chan []byte }
type pipeWriter struct{ ch chan []byte }

func newPipe() (*pipeReader, *pipeWriter) {
	ch := make(chan []byte, 256)
	return &pipeReader{ch}, &pipeWriter{ch}
}

func (r *pipeReader) Read(p []byte) (int, error) {
	data, ok := <-r.ch
	if !ok {
		return 0, nil
	}
	return copy(p, data), nil
}

func (w *pipeWriter) Write(p []byte) (int, error) {
	buf := make([]byte, len(p))
	copy(buf, p)
	w.ch <- buf
	return len(p), nil
}

func (w *pipeWriter) Close() error {
	close(w.ch)
	return nil
}
