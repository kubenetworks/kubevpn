package util

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/httpstream"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/kubectl/pkg/cmd/util"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// PortForwardPod establishes port forwarding to the specified pod using SPDY or WebSocket transport.
func PortForwardPod(config *rest.Config, clientset *rest.RESTClient, podName, namespace string, portPair []string, readyChan chan struct{}, stopChan <-chan struct{}, out, errOut io.Writer) error {
	url := clientset.
		Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").
		URL()
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		plog.G(context.Background()).Errorf("Create spdy roundtripper error: %v", err)
		return err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)
	// Legacy SPDY executor is default. If feature gate enabled, fallback
	// executor attempts websockets first--then SPDY.
	if util.RemoteCommandWebsockets.IsEnabled() {
		// WebSocketExecutor must be "GET" method as described in RFC 6455 Sec. 4.1 (page 17).
		websocketDialer, err := portforward.NewSPDYOverWebsocketDialer(url, config)
		if err != nil {
			return err
		}
		dialer = portforward.NewFallbackDialer(websocketDialer, dialer, httpstream.IsUpgradeFailure)
	}
	forwarder, err := portforward.New(dialer, portPair, stopChan, readyChan, out, errOut)
	if err != nil {
		return err
	}

	suppressExpectedPortForwardCloseErrors()
	defer forwarder.Close()

	errChan := make(chan error, 1)
	go func() {
		errChan <- forwarder.ForwardPorts()
	}()

	select {
	case err = <-errChan:
		return err
	case <-stopChan:
		return nil
	}
}

var portForwardErrorHandlerOnce sync.Once

func suppressExpectedPortForwardCloseErrors() {
	portForwardErrorHandlerOnce.Do(func() {
		prev := append([]utilruntime.ErrorHandler(nil), utilruntime.ErrorHandlers...)
		utilruntime.ErrorHandlers = []utilruntime.ErrorHandler{
			func(ctx context.Context, err error, msg string, keysAndValues ...any) {
				if err != nil {
					errMsg := strings.ToLower(err.Error())
					if strings.Contains(errMsg, "error closing listener: close tcp") &&
						strings.Contains(errMsg, "use of closed network connection") {
						return
					}
				}
				for _, handler := range prev {
					handler(ctx, err, msg, keysAndValues...)
				}
			},
		}
	})
}
