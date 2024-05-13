package util

import (
	"fmt"
	"testing"
	"time"
)

func TestChanClose(t *testing.T) {
	c := make(chan any)
	close(c)
	SafeWrite(c, nil)

	c = make(chan any)
	go func() {
		time.AfterFunc(time.Second*3, func() {
			close(c)
		})
	}()
	for a := range c {
		fmt.Printf("%v", a)
	}
}
