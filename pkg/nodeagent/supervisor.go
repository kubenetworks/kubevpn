package nodeagent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type launchSpec struct {
	workloadUID  string
	listenerPort int32
	adminPort    int32
	baseID       int32
}

func envoyLaunchSpec(workloadUID string, listenerPort int32) launchSpec {
	return launchSpec{
		workloadUID:  workloadUID,
		listenerPort: listenerPort,
		adminPort:    listenerPort + 5000,
		baseID:       listenerPort - controlplane.AgentListenerPortMin + 1,
	}
}

type envoyProcess struct {
	spec launchSpec
	cmd  *exec.Cmd
	done chan error
}

// Supervisor owns only local Envoy processes, not Kubernetes Jobs or pods.
type Supervisor struct {
	mu        sync.Mutex
	processes map[string]*envoyProcess
}

func newSupervisor() *Supervisor {
	return &Supervisor{processes: make(map[string]*envoyProcess)}
}

func (s *Supervisor) Sync(ctx context.Context, managerAddr string, attachments []controlplane.NodeAttachment) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	desired := make(map[string]launchSpec)
	for _, attachment := range attachments {
		desired[attachment.WorkloadUID] = envoyLaunchSpec(attachment.WorkloadUID, attachment.AgentListenerPort)
	}
	for uid, process := range s.processes {
		if _, ok := desired[uid]; !ok {
			s.stop(process)
			delete(s.processes, uid)
		}
	}
	namespaces := make(map[string]string, len(attachments))
	nodeIDs := make(map[string]string, len(attachments))
	for _, attachment := range attachments {
		namespaces[attachment.WorkloadUID] = attachment.Namespace
		nodeIDs[attachment.WorkloadUID] = attachment.NodeID
	}
	for uid, spec := range desired {
		if process, ok := s.processes[uid]; ok {
			select {
			case err := <-process.done:
				plog.G(ctx).Warnf("[node-agent] envoy for %s exited: %v", uid, err)
			default:
				continue
			}
		}
		process, err := s.start(ctx, managerAddr, namespaces[uid], nodeIDs[uid], spec)
		if err != nil {
			plog.G(ctx).Errorf("[node-agent] start envoy for %s failed: %v", uid, err)
			continue
		}
		s.processes[uid] = process
	}
	return nil
}

func (s *Supervisor) start(ctx context.Context, managerAddr, namespace, derivedNodeID string, spec launchSpec) (*envoyProcess, error) {
	bootstrap, err := inject.RenderEnvoyBootstrapWithAdmin(ctx, managerAddr, spec.adminPort)
	if err != nil {
		return nil, fmt.Errorf("render envoy bootstrap: %w", err)
	}
	configPath := filepath.Join("/tmp", fmt.Sprintf("kubevpn-nodeagent-%s.yaml", strings.NewReplacer("/", "_", ".", "_").Replace(spec.workloadUID)))
	if err := os.WriteFile(configPath, bootstrap, 0o600); err != nil {
		return nil, fmt.Errorf("write envoy bootstrap: %w", err)
	}
	nodeID := util.GenEnvoyUID(namespace, derivedNodeID)
	cmd := exec.CommandContext(ctx, "envoy", "-c", configPath,
		"--service-node", nodeID,
		"--service-cluster", nodeID,
		"--base-id", fmt.Sprintf("%d", spec.baseID),
		"--log-level", "info",
	)
	plog.G(ctx).Infof("[node-agent] starting envoy nodeID=%s admin=%d", nodeID, spec.adminPort)
	logPath := filepath.Join("/tmp", fmt.Sprintf("kubevpn-nodeagent-%s.log", strings.NewReplacer("/", "_", ".", "_").Replace(spec.workloadUID)))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open envoy log: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start envoy: %w", err)
	}
	process := &envoyProcess{spec: spec, cmd: cmd, done: make(chan error, 1)}
	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		process.done <- err
	}()
	return process, nil
}

func (s *Supervisor) stop(process *envoyProcess) {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return
	}
	_ = process.cmd.Process.Kill()
	select {
	case <-process.done:
	case <-time.After(5 * time.Second):
	}
}

func (s *Supervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for uid, process := range s.processes {
		s.stop(process)
		delete(s.processes, uid)
	}
}
