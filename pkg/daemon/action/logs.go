package action

import (
	"io"
	"log"
	"os"
	"strings"

	"github.com/hpcloud/tail"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Logs handles the Logs RPC, streaming the daemon's log output (both user and root) with optional follow mode.
func (svr *Server) Logs(resp rpc.Daemon_LogsServer) error {
	req, err := resp.Recv()
	if err != nil {
		return err
	}

	keep := makeLogFilter(req)
	line := int64(max(req.Lines, -req.Lines))
	sudoLine, sudoSize, err := seekToLastLine(config.GetDaemonLogPath(true), line)
	if err != nil {
		return err
	}
	userLine, userSize, err := seekToLastLine(config.GetDaemonLogPath(false), line)
	if err != nil {
		return err
	}
	err = recent(resp, sudoLine, userLine, keep)
	if err != nil {
		return err
	}

	if req.Follow {
		err = tee(resp, sudoSize, userSize, keep)
		if err != nil {
			return err
		}
	}
	return nil
}

// makeLogFilter builds a per-line predicate from the request's ConnectionID/Tun filters, with
// lenient semantics: for each set filter, a line is kept if it carries no such tag at all
// (shared/early/setup logs, or the user daemon which has no tun) OR its tag matches; a line tagged
// with a different value is dropped. The two filters are ANDed. Empty filters keep everything.
func makeLogFilter(req *rpc.LogRequest) func(string) bool {
	id, tun := req.GetConnectionID(), req.GetTun()
	if id == "" && tun == "" {
		return func(string) bool { return true }
	}
	return func(text string) bool {
		if id != "" && strings.Contains(text, LogFieldConnID+"=") && !hasField(text, LogFieldConnID, id) {
			return false
		}
		if tun != "" && strings.Contains(text, plog.FieldTun+"=") && !hasField(text, plog.FieldTun, tun) {
			return false
		}
		return true
	}
}

// hasField reports whether text contains the rendered field "key=val" as a whole token, i.e.
// followed by a field separator (space or "]") or end of line — so "connID=abc" does not match
// "connID=abcdef" and "tun=utun5" does not match "tun=utun50".
func hasField(text, key, val string) bool {
	needle := key + "=" + val
	for i := 0; i <= len(text); {
		j := strings.Index(text[i:], needle)
		if j < 0 {
			return false
		}
		end := i + j + len(needle)
		if end == len(text) || text[end] == ' ' || text[end] == ']' {
			return true
		}
		i = i + j + 1
	}
	return false
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

func sendLines(resp rpc.Daemon_LogsServer, t *tail.Tail, prefix string, keep func(string) bool) error {
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
			if !keep(line.Text) {
				continue
			}
			if err := resp.Send(&rpc.LogResponse{Message: prefix + line.Text + "\n"}); err != nil {
				return err
			}
		}
	}
}

func tee(resp rpc.Daemon_LogsServer, sudoOffset int64, userOffset int64, keep func(string) bool) error {
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
			if !keep(line.Text) {
				continue
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
			if !keep(line.Text) {
				continue
			}
			if err := resp.Send(&rpc.LogResponse{Message: "[ROOT] " + line.Text + "\n"}); err != nil {
				return err
			}
		}
	}
}

func recent(resp rpc.Daemon_LogsServer, sudoOffset int64, userOffset int64, keep func(string) bool) error {
	userFile, err := tail.TailFile(config.GetDaemonLogPath(false), newTailConfig(userOffset, false))
	if err != nil {
		return err
	}
	defer userFile.Stop()
	if err := sendLines(resp, userFile, "[USER] ", keep); err != nil {
		return err
	}

	sudoFile, err := tail.TailFile(config.GetDaemonLogPath(true), newTailConfig(sudoOffset, false))
	if err != nil {
		return err
	}
	defer sudoFile.Stop()
	return sendLines(resp, sudoFile, "[ROOT] ", keep)
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
	// Fewer lines than requested: start from the beginning, but report the real
	// file size so the follower tails from the current end, not byte 0.
	return 0, size, nil
}
