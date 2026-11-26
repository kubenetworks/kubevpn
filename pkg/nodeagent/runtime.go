package nodeagent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Options configures one node-local agent process.
type Options struct {
	Clientset        kubernetes.Interface
	ManagerNamespace string
	NodeName         string
	PodIP            string
	Image            string
}

// newConfigMapInformerFactory scopes list/watch to the manager ConfigMap. The
// manager Role uses resourceNames, which Kubernetes only honors for list/watch
// when the request includes this metadata.name field selector.
func newConfigMapInformerFactory(clientset kubernetes.Interface, namespace string) informers.SharedInformerFactory {
	return informers.NewSharedInformerFactoryWithOptions(
		clientset,
		0,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("metadata.name", config.ConfigMapPodTrafficManager).String()
		}),
	)
}

// Run starts the single node tunnel and converges local DNAT rules and Envoy
// processes from the manager's desired attachments.
func Run(ctx context.Context, opts Options) error {
	if opts.Clientset == nil || opts.ManagerNamespace == "" || opts.NodeName == "" || opts.PodIP == "" {
		return fmt.Errorf("clientset, manager namespace, node name and pod IP are required")
	}
	tunnelCtx, tunnelCancel := context.WithCancel(ctx)
	defer tunnelCancel()
	go maintainTunnel(tunnelCtx, opts)

	changes := make(chan struct{}, 1)
	factory := newConfigMapInformerFactory(opts.Clientset, opts.ManagerNamespace)
	informer := factory.Core().V1().ConfigMaps().Informer()
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { trigger(changes) },
		UpdateFunc: func(any, any) { trigger(changes) },
		DeleteFunc: func(any) { trigger(changes) },
	})
	if err != nil {
		return fmt.Errorf("watch traffic manager configmap: %w", err)
	}
	factory.Start(ctx.Done())
	supervisor := newSupervisor()
	defer supervisor.Stop()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ConvergeHostDNAT(cleanupCtx, execRunner{}, opts.PodIP, nil); err != nil {
			plog.G(cleanupCtx).Warnf("[node-agent] cleanup host DNAT rules: %v", err)
		}
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var lastSignature string
	reconcile := func() {
		signature, err := attachmentSignature(ctx, opts)
		if err != nil {
			plog.G(ctx).Warnf("[node-agent] compute attachment signature: %v", err)
			return
		}
		if signature == lastSignature {
			return
		}
		if err := reconcileAttachments(ctx, opts, supervisor); err != nil {
			plog.G(ctx).Warnf("[node-agent] reconcile attachments failed: %v", err)
			lastSignature = ""
			return
		}
		lastSignature = signature
	}
	trigger(changes)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changes:
			reconcile()
		case <-ticker.C:
			reconcile()
		}
	}
}

func attachmentSignature(ctx context.Context, opts Options) (string, error) {
	cm, err := opts.Clientset.CoreV1().ConfigMaps(opts.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read traffic manager configmap: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(cm.Data[config.KeyNodeAttachments]))), nil
}

func trigger(changes chan struct{}) {
	select {
	case changes <- struct{}{}:
	default:
	}
}

func reconcileAttachments(ctx context.Context, opts Options, supervisor *Supervisor) error {
	cm, err := opts.Clientset.CoreV1().ConfigMaps(opts.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read traffic manager configmap: %w", err)
	}
	var attachments []controlplane.NodeAttachment
	if data := cm.Data[config.KeyNodeAttachments]; data != "" {
		if err := yaml.Unmarshal([]byte(data), &attachments); err != nil {
			return fmt.Errorf("parse node attachments: %w", err)
		}
	}
	filtered := make([]controlplane.NodeAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment.NodeName == opts.NodeName && attachment.PodIP != "" {
			filtered = append(filtered, attachment)
			plog.G(ctx).Infof("[node-agent] reconcile %s/%s nodeID=%s podIP=%s admin=%d ports=%v", attachment.Namespace, attachment.WorkloadUID, attachment.NodeID, attachment.PodIP, attachment.AgentListenerPort, attachment.Ports)
		}
	}
	if err := ConvergeHostDNAT(ctx, execRunner{}, opts.PodIP, filtered); err != nil {
		return err
	}
	managerAddr := fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, opts.ManagerNamespace)
	return supervisor.Sync(ctx, managerAddr, filtered)
}

func buildTunnelCommand(ctx context.Context, executable, managerService, nodeName string) *exec.Cmd {
	managerAddr := fmt.Sprintf("%s:%d", managerService, config.PortTCP)
	url := fmt.Sprintf("tun:/tcp://%s?route=%s&agent=%s", managerAddr, config.CIDR.String(), nodeName)
	cmd := exec.CommandContext(ctx, executable, "server", "--debug", "-l", url)
	cmd.Env = append(os.Environ(), "TrafficManagerService="+managerService)
	return cmd
}

func maintainTunnel(ctx context.Context, opts Options) {
	backoff := time.Second
	for ctx.Err() == nil {
		executable, err := os.Executable()
		if err != nil {
			plog.G(ctx).Errorf("[node-agent] locate executable: %v", err)
			return
		}
		managerService := fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, opts.ManagerNamespace)
		cmd := buildTunnelCommand(ctx, executable, managerService, opts.NodeName)
		logPath := "/tmp/kubevpn-nodeagent-tunnel.log"
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			plog.G(ctx).Errorf("[node-agent] open tunnel log: %v", err)
			return
		}
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		err = cmd.Start()
		if err == nil {
			plog.G(ctx).Infof("[node-agent] tunnel to %s started", managerService)
			err = cmd.Wait()
		}
		_ = logFile.Close()
		if ctx.Err() != nil {
			return
		}
		plog.G(ctx).Warnf("[node-agent] tunnel exited, reconnecting: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}
