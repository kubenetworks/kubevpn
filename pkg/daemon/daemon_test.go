package daemon

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func TestDamonClient(t *testing.T) {
	go func() {
		(&SvrOption{Port: 55555}).Start(context.Background())
	}()
	time.Sleep(time.Millisecond * 200)
	cancel, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	go func() {
		time.AfterFunc(time.Second*2, func() {
			cancelFunc()
		})
	}()
	connect, err := GetClient(false).Connect(cancel, &rpc.ConnectRequest{})
	if err != nil {
		t.Error(err)
	}
	connect.Recv()
}

func TestDamonClientStream(t *testing.T) {
	go func() {
		(&SvrOption{Port: 55555}).Start(context.Background())
	}()
	time.Sleep(time.Millisecond * 200)
	cancel, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	go func() {
		time.AfterFunc(time.Second*10, func() {
			cancelFunc()
		})
	}()
	_, err := GetClient(true).Connect(cancel, nil)
	if err != nil {
		t.Error(err)
	}
}

func TestMap(t *testing.T) {
	var m = map[string]string{"m": "mm"}

	for i := 0; i < 10000; i++ {
		go func(i int) {
			if i%2 == 0 {
				_ = m["m"]
			} /* else {
				m["m"] = "mm"
			}*/
		}(i)
	}

	time.Sleep(time.Second)
}

type sleep struct {
	Name string `json:"name"`
}

func (receiver sleep) HandleJson(ctx context.Context) (interface{}, error) {
	cancel, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	go func() {
		time.Sleep(time.Second * 10)
		cancelFunc()
	}()
	<-cancel.Done()
	return "well done", nil
}

func (receiver sleep) HandleStream(ctx context.Context, resp io.Writer) error {
	cancelCtx, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	go func() {
		resp.Write([]byte(("hi client\n")))
		time.Sleep(time.Second)
		resp.Write([]byte(("i am doing that, please wait...\n")))
		time.Sleep(time.Second)
		resp.Write([]byte(("i am doing that, please wait...\n")))
		time.Sleep(time.Second)
		resp.Write([]byte("i am done\n"))
		cancelFunc()
	}()
	<-cancelCtx.Done()
	return nil
}
