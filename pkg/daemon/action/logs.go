package action

import (
	"io"
	"log"
	"os"

	"github.com/hpcloud/tail"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func (svr *Server) Logs(resp rpc.Daemon_LogsServer) error {
	req, err := resp.Recv()
	if err != nil {
		return err
	}

	// only show latest N lines
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

func tee(resp rpc.Daemon_LogsServer, sudoLine int64, userLine int64) error {
	// FATAL -- cannot set ReOpen without Follow.
	sudoConfig := tail.Config{
		Follow:    true,
		ReOpen:    true,
		MustExist: true,
		Logger:    log.New(io.Discard, "", log.LstdFlags),
		Location:  &tail.SeekInfo{Offset: sudoLine, Whence: io.SeekStart},
	}
	userConfig := tail.Config{
		Follow:    true,
		ReOpen:    true,
		MustExist: true,
		Logger:    log.New(io.Discard, "", log.LstdFlags),
		Location:  &tail.SeekInfo{Offset: userLine, Whence: io.SeekStart},
	}
	sudoFile, err := tail.TailFile(config.GetDaemonLogPath(true), sudoConfig)
	if err != nil {
		return err
	}
	defer sudoFile.Stop()
	userFile, err := tail.TailFile(config.GetDaemonLogPath(false), userConfig)
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

			err = resp.Send(&rpc.LogResponse{Message: "[USER] " + line.Text + "\n"})
			if err != nil {
				return err
			}
		case line, ok := <-sudoFile.Lines:
			if !ok {
				return nil
			}
			if line.Err != nil {
				return err
			}

			err = resp.Send(&rpc.LogResponse{Message: "[ROOT] " + line.Text + "\n"})
			if err != nil {
				return err
			}
		}
	}
}

func recent(resp rpc.Daemon_LogsServer, sudoLine int64, userLine int64) error {
	sudoConfig := tail.Config{
		Follow:    false,
		ReOpen:    false,
		MustExist: true,
		Logger:    log.New(io.Discard, "", log.LstdFlags),
		Location:  &tail.SeekInfo{Offset: sudoLine, Whence: io.SeekStart},
	}
	userConfig := tail.Config{
		Follow:    false,
		ReOpen:    false,
		MustExist: true,
		Logger:    log.New(io.Discard, "", log.LstdFlags),
		Location:  &tail.SeekInfo{Offset: userLine, Whence: io.SeekStart},
	}
	sudoFile, err := tail.TailFile(config.GetDaemonLogPath(true), sudoConfig)
	if err != nil {
		return err
	}
	defer sudoFile.Stop()
	userFile, err := tail.TailFile(config.GetDaemonLogPath(false), userConfig)
	if err != nil {
		return err
	}
	defer userFile.Stop()
userOut:
	for {
		select {
		case <-resp.Context().Done():
			return nil
		case line, ok := <-userFile.Lines:
			if !ok {
				break userOut
			}
			if line.Err != nil {
				return line.Err
			}

			err = resp.Send(&rpc.LogResponse{Message: "[USER] " + line.Text + "\n"})
			if err != nil {
				return err
			}
		}
	}
sudoOut:
	for {
		select {
		case <-resp.Context().Done():
			return nil
		case line, ok := <-sudoFile.Lines:
			if !ok {
				break sudoOut
			}
			if line.Err != nil {
				return line.Err
			}

			err = resp.Send(&rpc.LogResponse{Message: "[ROOT] " + line.Text + "\n"})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

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
	bufSize := int64(4096)
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
