package localproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type kubePortForwarder struct {
	config     *rest.Config
	restClient *rest.RESTClient
}

func NewPodDialer(config *rest.Config, clientset *kubernetes.Clientset) PodDialer {
	restClient, _ := clientset.CoreV1().RESTClient().(*rest.RESTClient)
	return &kubePortForwarder{
		config:     config,
		restClient: restClient,
	}
}

func (k *kubePortForwarder) DialPod(ctx context.Context, namespace, podName string, port int32) (net.Conn, error) {
	if k.restClient == nil {
		return nil, fmt.Errorf("pod port-forward requires REST client")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	stopChan := make(chan struct{})
	readyChan := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- util.PortForwardPod(
			k.config,
			k.restClient,
			podName,
			namespace,
			[]string{fmt.Sprintf("%d:%d", localPort, port)},
			readyChan,
			stopChan,
			io.Discard,
			io.Discard,
		)
	}()

	select {
	case <-readyChan:
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		close(stopChan)
		return nil, ctx.Err()
	}

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if err != nil {
		close(stopChan)
		return nil, err
	}
	return &forwardedConn{Conn: conn, stop: stopChan}, nil
}

type forwardedConn struct {
	net.Conn
	stop chan struct{}
	once sync.Once
}

func (c *forwardedConn) Close() error {
	c.once.Do(func() {
		close(c.stop)
	})
	return c.Conn.Close()
}
