package webhook

import (
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
	putil "github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func Main(manager *dhcp.Manager) error {
	h := &admissionReviewHandler{dhcp: manager}
	http.HandleFunc("/pods", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, newDelegateToV1AdmitHandler(h.admitPods))
	})
	http.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	tlsConfig, err := putil.GetTlsServerConfig(nil)
	if err != nil {
		return fmt.Errorf("failed to load tls certificate: %v", err)
	}

	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 100
	server := &http.Server{
		Addr:      fmt.Sprintf(":%d", 80),
		TLSConfig: &tls.Config{Certificates: tlsConfig.Certificates},
	}
	return server.ListenAndServeTLS("", "")
}
