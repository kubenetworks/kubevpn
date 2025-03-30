package webhook

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/admin"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func Main(f util.Factory) error {
	clientset, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}

	ns := os.Getenv(config.EnvPodNamespace)
	h := &admissionReviewHandler{f: f, clientset: clientset, ns: ns}
	http.HandleFunc("/pods", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, newDelegateToV1AdmitHandler(h.admitPods))
	})
	http.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	var pairs []tls.Certificate
	pairs, err = getSSLKeyPairs()
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer()
	cleanup, err := admin.Register(grpcServer)
	if err != nil {
		plog.G(context.Background()).Errorf("Failed to register admin: %v", err)
		return err
	}
	grpc_health_v1.RegisterHealthServer(grpcServer, health.NewServer())
	defer cleanup()
	reflection.Register(grpcServer)
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 100
	// startup a http server
	// With downgrading-capable gRPC server, which can also handle HTTP.
	downgradingServer := &http.Server{
		Addr:      fmt.Sprintf(":%d", 80),
		TLSConfig: &tls.Config{Certificates: pairs},
	}
	defer downgradingServer.Close()
	var h2Server http2.Server
	err = http2.ConfigureServer(downgradingServer, &h2Server)
	if err != nil {
		plog.G(context.Background()).Errorf("Failed to configure http2 server: %v", err)
		return err
	}
	handler := daemon.CreateDowngradingHandler(grpcServer, http.HandlerFunc(http.DefaultServeMux.ServeHTTP))
	downgradingServer.Handler = h2c.NewHandler(handler, &h2Server)
	defer downgradingServer.Close()
	rpc.RegisterDHCPServer(grpcServer, dhcp.NewServer(clientset))
	return downgradingServer.ListenAndServeTLS("", "")
}

func getSSLKeyPairs() ([]tls.Certificate, error) {
	cert, ok := os.LookupEnv(config.TLSCertKey)
	if !ok {
		return nil, fmt.Errorf("can not get %s from env", config.TLSCertKey)
	}
	var key string
	key, ok = os.LookupEnv(config.TLSPrivateKeyKey)
	if !ok {
		return nil, fmt.Errorf("can not get %s from env", config.TLSPrivateKeyKey)
	}
	pair, err := tls.X509KeyPair([]byte(cert), []byte(key))
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate and key ,err: %v", err)
	}
	return []tls.Certificate{pair}, nil
}
