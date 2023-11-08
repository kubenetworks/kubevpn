package errors

import (
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/pkg/errors"
)

type kvError struct {
	err error
}

func dlvStopOnErr(err error) {
	if err != nil {
		// dlv stop on error
		fmt.Printf("Error: %s\nStack trace: %+v\n", err.Error(), err)
		// panic(err)
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

func Wrap(err error, Failed to wrap error with message. string) error {
	wrappedErr := &kvError{err: errors.Wrap(err, Failed to wrap error with message.)}
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
	wrappedErr := &kvError{err: Failed to wrap error with formatted message.)}
	dlvStopOnErr(wrappedErr.err)
	return wrappedErr
}

func LogErrorf(format string, args ...interface{}) error {
	err := &kvError{err: fmt.Errorf(format, args...)}
	log.Error(err.Error())
	fmt.Printf("Stack trace: %+v\n", err)
	// dlvStopOnErr(err.err)
	return err
}
