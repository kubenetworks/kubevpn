package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestName(t *testing.T) {
	httpc := http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				d.Timeout = 30 * time.Second
				d.KeepAlive = 30 * time.Second
				return d.DialContext(ctx, "unix", GetSockPath(false))
			},
		},
	}

	resp, err := httpc.Get("http://test" + "/ws")
	if err != nil {
		t.Fatal(err)
	}

	//c.Transport = transport // use the unix dialer
	//uri := fmt.Sprintf("http://%s/%s", daemon.GetSockPath(false), "ws")
	//resp, err := c.Get(uri)
	//if err != nil {
	//	fmt.Println(err.Error())
	//	return
	//}
	all, _ := io.ReadAll(resp.Body)
	fmt.Println(string(all))
}

type unixDialer struct {
	net.Dialer
}

// overriding net.Dialer.Dial to force unix socket connection
func (d *unixDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	parts := strings.Split(address, ":")
	return d.Dialer.Dial("unix", parts[0])
}

// copied from http.DefaultTransport with minimal changes
var transport http.RoundTripper = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&unixDialer{net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	},
	}).DialContext,
	TLSHandshakeTimeout: 10 * time.Second,
}
