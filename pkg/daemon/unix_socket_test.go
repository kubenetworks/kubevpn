package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func TestHttpOverUnix(t *testing.T) {
	file := filepath.Join(os.TempDir(), "kubevpn.socks")
	client := http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSHandshakeTimeout: 10 * time.Second,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				d.Timeout = 30 * time.Second
				d.KeepAlive = 30 * time.Second
				return d.DialContext(ctx, "unix", file)
			},
		},
	}

	go func() {
		listener, err := net.Listen("unix", file)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		downgradingServer := &http.Server{}
		defer downgradingServer.Close()
		var h2Server http2.Server
		err = http2.ConfigureServer(downgradingServer, &h2Server)
		if err != nil {
			t.Fatal(err)
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
