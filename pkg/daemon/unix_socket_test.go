package daemon

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func TestHttpOverUnix(t *testing.T) {
	temp, err := os.CreateTemp("", "kubevpn.socks")
	if err != nil {
		t.Fatal(err)
	}
	err = os.Remove(temp.Name())
	if err != nil {
		t.Fatal(err)
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
	t.Log(string(all))
}

func TestConnectionRefuse(t *testing.T) {
	ctx := context.Background()
	temp, err := os.CreateTemp("", "*.sock")
	if err != nil {
		t.Fatal(err)
	}
	_ = temp.Close()
	_ = os.Remove(temp.Name())

	socketPath := temp.Name()
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Failed to listen on unix socket %s: %v", socketPath, err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	defer listener.Close()

	// 1) normal
	conn, err := grpc.Dial("unix:"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	healthClient := grpc_health_v1.NewHealthClient(conn)
	_, err = healthClient.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{})
	if err == nil {
		t.Fatal("impossible")
	}
	t.Log(status.Code(err).String())
	t.Log(status.Convert(err).Message())

	// 2) close listener
	_ = conn.Close()
	_ = listener.Close()
	conn, err = grpc.Dial("unix:"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	healthClient = grpc_health_v1.NewHealthClient(conn)
	_, err = healthClient.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{})
	if err == nil {
		t.Fatal("impossible")
	}
	t.Log(status.Code(err).String())
	t.Log(status.Convert(err).Message())

	// 3) remove unix socket file
	err = os.Remove(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	conn, err = grpc.Dial("unix:"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	healthClient = grpc_health_v1.NewHealthClient(conn)
	_, err = healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err == nil {
		t.Fatal("impossible")
	}
	t.Log(status.Code(err).String())
	t.Log(status.Convert(err).Message())
}
