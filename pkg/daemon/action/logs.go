package action

import (
	"bufio"
	"io"
	"log"
	"os"

	"github.com/hpcloud/tail"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func (svr *Server) Logs(req *rpc.LogRequest, resp rpc.Daemon_LogsServer) error {
	path := GetDaemonLogPath()

	lines, err2 := countLines(path)
	if err2 != nil {
		return err2
	}

	// only show latest N lines
	lines -= req.Lines

	config := tail.Config{Follow: req.Follow, ReOpen: false, MustExist: true, Logger: log.New(io.Discard, "", log.LstdFlags)}
	if !req.Follow {
		// FATAL -- cannot set ReOpen without Follow.
		config.ReOpen = false
	}
	file, err := tail.TailFile(path, config)
	if err != nil {
		return err
	}
	defer file.Stop()
	for {
		select {
		case <-resp.Context().Done():
			return nil
		case line, ok := <-file.Lines:
			if !ok {
				return nil
			}
			if line.Err != nil {
				return err
			}

			if lines--; lines >= 0 {
				continue
			}

			err = resp.Send(&rpc.LogResponse{Message: line.Text})
			if err != nil {
				return err
			}
		}
	}
}

func countLines(filename string) (int32, error) {
	file, err := os.Open(filename)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineCount := int32(0)

	for scanner.Scan() {
		lineCount++
	}

	if err = scanner.Err(); err != nil {
		return 0, err
	}

	return lineCount, nil
}
