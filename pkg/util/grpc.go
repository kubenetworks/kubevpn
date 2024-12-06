package util

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Printable interface {
	GetMessage() string
}

func PrintGRPCStream[T any](clientStream grpc.ClientStream, writers ...io.Writer) error {
	var out io.Writer = os.Stdout
	for _, writer := range writers {
		out = writer
		break
	}

	for {
		var t = new(T)
		err := clientStream.RecvMsg(t)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if status.Code(err) == codes.Canceled {
			return nil
		}
		if err != nil {
			return err
		}
		if out == nil {
			continue
		}
		if p, ok := any(t).(Printable); ok {
			_, _ = fmt.Fprintf(out, p.GetMessage())
		} else {
			buf, _ := json.Marshal(t)
			_, _ = fmt.Fprintf(out, string(buf))
		}
	}
}

func CopyGRPCStream[T any](r grpc.ClientStream, w grpc.ServerStream) error {
	for {
		var t = new(T)
		err := r.RecvMsg(t)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		err = w.SendMsg(t)
		if err != nil {
			return err
		}
	}
}

func CopyAndConvertGRPCStream[I any, O any](r grpc.ClientStream, w grpc.ServerStream, convert func(*I) *O) error {
	for {
		var i = new(I)
		err := r.RecvMsg(i)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		o := convert(i)
		err = w.SendMsg(o)
		if err != nil {
			return err
		}
	}
}
