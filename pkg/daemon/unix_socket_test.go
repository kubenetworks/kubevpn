package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func TestHttpOverUnix(t *testing.T) {
	temp, err2 := os.CreateTemp("", "kubevpn.socks")
	if err2 != nil {
		t.Fatal(err2)
	}
	err2 = temp.Close()
	if err2 != nil {
		t.Fatal(err2)
	}
	err2 = os.Remove(temp.Name())
	if err2 != nil {
		t.Fatal(err2)
	}
	client := http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSHandshakeTimeout: 10 * time.Second,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				d.Timeout = 30 * time.Second
				d.KeepAlive = 30 * time.Second
				return d.DialContext(ctx, "unix", temp.Name())
			},
		},
	}

	go func() {
		listener, err := net.Listen("unix", temp.Name())
		if err != nil {
			t.Error(err)
			return
		}
		defer listener.Close()
		downgradingServer := &http.Server{}
		defer downgradingServer.Close()
		var h2Server http2.Server
		err = http2.ConfigureServer(downgradingServer, &h2Server)
		if err != nil {
			t.Error(err)
			return
		}
		downgradingServer.Handler = h2c.NewHandler(http.HandlerFunc(http.DefaultServeMux.ServeHTTP), &h2Server)
		downgradingServer.Serve(listener)
	}()

	time.Sleep(time.Second * 2)

	//var resp *http.Response
	resp, err := client.Get("http://test" + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	all, _ := io.ReadAll(resp.Body)
	fmt.Println(string(all))
}
