package util

import (
	"fmt"
	"path/filepath"

	log "github.com/sirupsen/logrus"
)

func InitLogger(debug bool) {
	if debug {
		log.SetLevel(log.DebugLevel)
	}
	log.SetReportCaller(true)
	log.SetFormatter(&format{})
}

func InitLoggerForServer(debug bool) {
	if debug {
		log.SetLevel(log.DebugLevel)
	}
	log.SetReportCaller(true)
	log.SetFormatter(&serverFormat{})
}

type format struct {
	log.Formatter
}

// Format
// message
func (*format) Format(e *log.Entry) ([]byte, error) {
	return []byte(fmt.Sprintf("%s\n", e.Message)), nil
}

type serverFormat struct {
	log.Formatter
}

// Format
// same like log.SetFlags(log.LstdFlags | log.Lshortfile)
// 2009/01/23 01:23:23 d.go:23: message
func (*serverFormat) Format(e *log.Entry) ([]byte, error) {
	return []byte(
		fmt.Sprintf("%s %s:%d: %s\n",
			e.Time.Format("2006/01/02 15:04:05"),
			filepath.Base(e.Caller.File),
			e.Caller.Line,
			e.Message,
		)), nil
}
