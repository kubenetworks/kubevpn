package nodeagent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	factory := informers.NewSharedInformerFactoryWithOptions(opts.Clientset, 0, informers.WithNamespace(opts.ManagerNamespace))
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
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	trigger(changes)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changes:
			if err := reconcileAttachments(ctx, opts, supervisor); err != nil {
				plog.G(ctx).Warnf("[node-agent] reconcile attachments failed: %v", err)
			}
		case <-ticker.C:
			if err := reconcileAttachments(ctx, opts, supervisor); err != nil {
				plog.G(ctx).Warnf("[node-agent] reconcile attachments failed: %v", err)
			}
		}
	}
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
		}
	}
	if err := ConvergeHostDNAT(ctx, execRunner{}, opts.PodIP, filtered); err != nil {
		return err
	}
	managerAddr := fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, opts.ManagerNamespace)
	return supervisor.Sync(ctx, managerAddr, filtered)
}

func maintainTunnel(ctx context.Context, opts Options) {
	backoff := time.Second
	for ctx.Err() == nil {
		executable, err := os.Executable()
		if err != nil {
			plog.G(ctx).Errorf("[node-agent] locate executable: %v", err)
			return
		}
		managerAddr := fmt.Sprintf("%s.%s:%d", config.ConfigMapPodTrafficManager, opts.ManagerNamespace, config.PortTCP)
		url := fmt.Sprintf("tun:/tcp://%s?route=%s&agent=%s", managerAddr, config.CIDR.String(), opts.NodeName)
		cmd := exec.CommandContext(ctx, executable, "server", "--debug", "-l", url)
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
			plog.G(ctx).Infof("[node-agent] tunnel to %s started", managerAddr)
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
