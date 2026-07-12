package action

import (
	"io"
	"log"
	"os"

	"github.com/hpcloud/tail"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// Logs handles the Logs RPC, streaming the daemon's log output (both user and root) with optional follow mode.
func (svr *Server) Logs(resp rpc.Daemon_LogsServer) error {
	req, err := resp.Recv()
	if err != nil {
		return err
	}

	line := int64(max(req.Lines, -req.Lines))
	sudoLine, sudoSize, err := seekToLastLine(config.GetDaemonLogPath(true), line)
	if err != nil {
		return err
	}
	userLine, userSize, err := seekToLastLine(config.GetDaemonLogPath(false), line)
	if err != nil {
		return err
	}
	err = recent(resp, sudoLine, userLine)
	if err != nil {
		return err
	}

	if req.Follow {
		err = tee(resp, sudoSize, userSize)
		if err != nil {
			return err
		}
	}
	return nil
}

func newTailConfig(offset int64, follow bool) tail.Config {
	return tail.Config{
		Follow:    follow,
		ReOpen:    follow,
		MustExist: true,
		Logger:    log.New(io.Discard, "", log.LstdFlags),
		Location:  &tail.SeekInfo{Offset: offset, Whence: io.SeekStart},
	}
}

func sendLines(resp rpc.Daemon_LogsServer, t *tail.Tail, prefix string) error {
	for {
		select {
		case <-resp.Context().Done():
			return nil
		case line, ok := <-t.Lines:
			if !ok {
				return nil
			}
			if line.Err != nil {
				return line.Err
			}
			if err := resp.Send(&rpc.LogResponse{Message: prefix + line.Text + "\n"}); err != nil {
				return err
			}
		}
	}
}

func tee(resp rpc.Daemon_LogsServer, sudoOffset int64, userOffset int64) error {
	sudoFile, err := tail.TailFile(config.GetDaemonLogPath(true), newTailConfig(sudoOffset, true))
	if err != nil {
		return err
	}
	defer sudoFile.Stop()
	userFile, err := tail.TailFile(config.GetDaemonLogPath(false), newTailConfig(userOffset, true))
	if err != nil {
		return err
	}
	defer userFile.Stop()
	for {
		select {
		case <-resp.Context().Done():
			return nil
		case line, ok := <-userFile.Lines:
			if !ok {
				return nil
			}
			if line.Err != nil {
				return line.Err
			}
			if err := resp.Send(&rpc.LogResponse{Message: "[USER] " + line.Text + "\n"}); err != nil {
				return err
			}
		case line, ok := <-sudoFile.Lines:
			if !ok {
				return nil
			}
			if line.Err != nil {
				return line.Err
			}
			if err := resp.Send(&rpc.LogResponse{Message: "[ROOT] " + line.Text + "\n"}); err != nil {
				return err
			}
		}
	}
}

func recent(resp rpc.Daemon_LogsServer, sudoOffset int64, userOffset int64) error {
	userFile, err := tail.TailFile(config.GetDaemonLogPath(false), newTailConfig(userOffset, false))
	if err != nil {
		return err
	}
	defer userFile.Stop()
	if err := sendLines(resp, userFile, "[USER] "); err != nil {
		return err
	}

	sudoFile, err := tail.TailFile(config.GetDaemonLogPath(true), newTailConfig(sudoOffset, false))
	if err != nil {
		return err
	}
	defer sudoFile.Stop()
	return sendLines(resp, sudoFile, "[ROOT] ")
}

// tailBlockSize is the chunk size used when scanning a log file backwards for the last N lines.
const tailBlockSize = 4096

func seekToLastLine(filename string, lines int64) (int64, int64, error) {
	file, err := os.Open(filename)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return 0, 0, err
	}
	size := stat.Size()
	bufSize := int64(tailBlockSize)
	lineCount := int64(0)
	remaining := size

	for remaining > 0 {
		chunkSize := bufSize
		if remaining < bufSize {
			chunkSize = remaining
		}
		pos := remaining - chunkSize
		_, err = file.Seek(pos, io.SeekStart)
		if err != nil {
			return 0, 0, err
		}

		buf := make([]byte, chunkSize)
		_, err = file.Read(buf)
		if err != nil {
			return 0, 0, err
		}

		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				lineCount++
				if lineCount > lines {
					targetPos := pos + int64(i) + 1
					return targetPos, size, nil
				}
			}
		}
		remaining -= chunkSize
	}
	return 0, 0, nil
}
