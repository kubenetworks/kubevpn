package errors

import (
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/pkg/errors"
)

type kvError struct {
	err error
}

// for dlv breakpoint
// break dlvStopOnErr
func dlvStopOnErr(err error) {
	if err != nil {
		log.Debugf("Error: %s\nStack trace: %+v\n", err.Error(), err)
	}
}

func (e *kvError) Error() string {
	return e.err.Error()
}

func New(message string) error {
	err := &kvError{err: errors.New(message)}
	dlvStopOnErr(err.err)
	return err
}

func Wrap(err error, msg string) error {
	wrappedErr := &kvError{err: errors.Wrap(err, msg)}
	dlvStopOnErr(wrappedErr.err)
	return wrappedErr
}

func Is(err, target error) bool {
	return errors.Is(err, target)
}

func Errorf(format string, args ...interface{}) error {
	err := &kvError{err: fmt.Errorf(format, args...)}
	dlvStopOnErr(err.err)
	return err
}

func Wrapf(err error, format string, args ...interface{}) error {
	wrappedErr := &kvError{err: errors.Wrapf(err, format, args...)}
	dlvStopOnErr(wrappedErr.err)
	return wrappedErr
}

func LogErrorf(format string, args ...interface{}) error {
	err := &kvError{err: fmt.Errorf(format, args...)}
	log.Error(err.Error())
	log.Debugf("Stack trace: %+v\n", err)
	return err
}
