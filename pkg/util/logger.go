package util

import (
	"fmt"
	"path/filepath"
	"runtime"

	log "github.com/sirupsen/logrus"
	"k8s.io/utils/ptr"
)

func InitLoggerForClient(debug bool) {
	if debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.SetReportCaller(false)
	log.SetFormatter(&format{})
}

func InitLoggerForServer(debug bool) {
	if debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
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
	// e.Caller maybe is nil, because pkg/handler/connect.go:252
	return []byte(
		fmt.Sprintf("%s %s:%d %s: %s\n",
			e.Time.Format("2006-01-02 15:04:05"),
			filepath.Base(ptr.Deref(e.Caller, runtime.Frame{}).File),
			ptr.Deref(e.Caller, runtime.Frame{}).Line,
			e.Level.String(),
			e.Message,
		)), nil
}
