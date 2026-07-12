package handler

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

func createOutboundPod(ctx context.Context, clientset kubernetes.Interface, namespace, image, imagePullSecretName string) (err error) {
	var exists bool
	exists, err = util.DetectPodExists(ctx, clientset, namespace)
	if err != nil {
		return err
	}
	if exists {
		// A running manager pod is present, but make sure its Deployment still
		// exists before treating the manager as installed. `kubevpn uninstall`
		// deletes the Deployment with GracePeriodSeconds=0 while its pod may
		// briefly linger (cascade GC lag): the pod still reports Running with no
		// deletionTimestamp, so DetectPodExists returns true. Skipping creation
		// here would then make the next step (UpgradeDeploy → Deployment Get)
		// fail with NotFound. Re-create from scratch when the Deployment is gone.
		_, derr := clientset.AppsV1().Deployments(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if derr == nil {
			plog.StepDone(ctx, "Using traffic manager in namespace %q", namespace)
			return nil
		}
		if !apierrors.IsNotFound(derr) {
			return fmt.Errorf("failed to get deployment %s: %w", config.ConfigMapPodTrafficManager, derr)
		}
	}

	defer func() {
		if err != nil {
			cleanupTrafficManagerResources(context.Background(), clientset, namespace)
		}
	}()
	cleanupTrafficManagerResources(ctx, clientset, namespace)

	// 1) label namespace
	plog.StepStart(ctx, "Labeling namespace")
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get namespace %s: %w", namespace, err)
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels["ns"] = namespace
	if _, err = clientset.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to label namespace %s: %w", namespace, err)
	}
	plog.StepDone(ctx, "Labeled namespace %q", namespace)

	// 2) create serviceAccount
	plog.StepStart(ctx, "Creating ServiceAccount")
	if _, err = clientset.CoreV1().ServiceAccounts(namespace).Create(ctx, genServiceAccount(namespace), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create service account: %w", err)
	}
	plog.StepDone(ctx, "Created ServiceAccount %q", config.ConfigMapPodTrafficManager)

	// 3) create role
	plog.StepStart(ctx, "Creating Role")
	if _, err = clientset.RbacV1().Roles(namespace).Create(ctx, genRole(namespace), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create role: %w", err)
	}
	plog.StepDone(ctx, "Created Role %q", config.ConfigMapPodTrafficManager)

	// 4) create roleBinding
	plog.StepStart(ctx, "Creating RoleBinding")
	if _, err = clientset.RbacV1().RoleBindings(namespace).Create(ctx, genRoleBinding(namespace), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create role binding: %w", err)
	}
	plog.StepDone(ctx, "Created RoleBinding %q", config.ConfigMapPodTrafficManager)

	// 5) create service
	plog.StepStart(ctx, "Creating Service")
	if _, err = clientset.CoreV1().Services(namespace).Create(ctx, genService(namespace), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create service: %w", err)
	}
	plog.StepDone(ctx, "Created Service %q", config.ConfigMapPodTrafficManager)

	// 6) create configMap
	plog.StepStart(ctx, "Creating ConfigMap")
	_, err = clientset.CoreV1().ConfigMaps(namespace).Create(ctx, &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Data: map[string]string{
			config.KeyEnvoy:        "",
			config.KeyTunIPPool:    "",
			config.KeyClusterCIDRs: "",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create config map: %w", err)
	}
	plog.StepDone(ctx, "Created ConfigMap %q", config.ConfigMapPodTrafficManager)

	// 7) create TLS secret (not surfaced as a step)
	if _, err = createTLSSecret(ctx, clientset, namespace); err != nil {
		return err
	}

	// 8) create deployment
	plog.StepStart(ctx, "Creating Deployment")
	deploy := genDeploySpec(namespace, image, imagePullSecretName)
	deploy, err = clientset.AppsV1().Deployments(namespace).Create(ctx, deploy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create deployment for %s: %w", config.ConfigMapPodTrafficManager, err)
	}
	plog.StepDone(ctx, "Created Deployment %q", config.ConfigMapPodTrafficManager)

	_, selector, err := polymorphichelpers.SelectorsForObject(deploy)
	if err != nil {
		return err
	}
	if err = WaitPodReady(ctx, clientset.CoreV1().Pods(namespace), selector.String(), "traffic manager"); err != nil {
		return err
	}
	plog.StepDone(ctx, "Traffic manager ready in namespace %q", namespace)
	return nil
}

// WaitPodReady blocks until a pod matching labelSelector is ready. When stepName
// is non-empty the wait is shown as a single in-place spinner line that updates
// with a one-line pod status ("Waiting for <stepName> pod (<summary>)"); when
// empty (callers that own their own step, e.g. sync, and tests) the status is
// logged at Debug instead. The full multi-line status table is kept only for the
// timeout error, so the reason survives without --debug.
func WaitPodReady(ctx context.Context, clientset corev1.PodInterface, labelSelector, stepName string) error {
	const (
		podReadyTimeout      = 60 * time.Minute
		podReadyPollInterval = 3 * time.Second
	)
	var isPodReady bool
	var lastSummary, lastDetail string
	if stepName != "" {
		plog.StepStart(ctx, fmt.Sprintf("Waiting for %s pod", stepName))
	}
	ctx2, cancelFunc := context.WithTimeout(ctx, podReadyTimeout)
	defer cancelFunc()
	wait.UntilWithContext(ctx2, func(ctx context.Context) {
		podList, err := clientset.List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return
		}
		if len(podList.Items) == 0 {
			return
		}
		for _, pod := range podList.Items {
			if podutils.IsPodReady(&pod) && pod.DeletionTimestamp == nil {
				isPodReady = true
				cancelFunc()
				return
			}
			// Only react when the one-line summary changes, to avoid needless
			// repaints / log lines.
			summary := util.PodStatusSummary(&pod)
			if summary == lastSummary {
				continue
			}
			lastSummary = summary
			var sb bytes.Buffer
			util.PrintStatus(&pod, &sb)
			lastDetail = sb.String()
			if stepName != "" {
				// Live one-line status, in place on the spinner.
				plog.StepStart(ctx, fmt.Sprintf("Waiting for %s pod (%s)", stepName, summary))
			} else {
				plog.G(ctx).Debugf("%s", summary)
			}
		}
	}, podReadyPollInterval)
	if !isPodReady {
		// An image that cannot be pulled is a distinct, actionable failure.
		cause := config.ErrTrafficManagerTimeout
		if strings.Contains(lastDetail, "ImagePullBackOff") || strings.Contains(lastDetail, "ErrImagePull") {
			cause = config.ErrImagePull
		}
		if lastDetail != "" {
			// Carry the full last status so the timeout reason (e.g.
			// Unschedulable / Insufficient memory) is visible without --debug.
			return fmt.Errorf("wait for pod ready timeout, last status:\n%s: %w", lastDetail, cause)
		}
		return fmt.Errorf("wait for pod ready timeout: %w", cause)
	}
	return nil
}

// createTLSSecret generates a TLS certificate and creates the corresponding
// Secret in the cluster.
// Note: v1.SecretTypeOpaque is used instead of v1.SecretTypeTls because TLS
// secret keys (tls.crt, tls.key) contain dots which are invalid as shell
// environment variable names, and the secret is mounted as env vars.
func createTLSSecret(ctx context.Context, clientset kubernetes.Interface, namespace string) (crt []byte, err error) {
	var key, host []byte
	crt, key, host, err = netutil.GenTLSCert(ctx, namespace)
	if err != nil {
		return nil, err
	}
	_, err = clientset.CoreV1().Secrets(namespace).Create(ctx, genSecret(namespace, crt, key, host), metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Creating secret error: %v", err)
		return nil, err
	}
	return crt, nil
}

// ensureRouteRBAC creates (idempotently, best-effort) the namespaced Role + RoleBinding
// that lets the traffic manager's ServiceAccount list/watch pods and services in the
// workload namespace, enabling server-side route/service discovery. It never fails the
// connect: if the RBAC cannot be created the manager returns Enabled=false and the client
// degrades to CIDR-only routing.
func ensureRouteRBAC(ctx context.Context, clientset kubernetes.Interface, workloadNamespace, managerNamespace string) {
	if workloadNamespace == "" {
		workloadNamespace = managerNamespace
	}
	if _, err := clientset.RbacV1().Roles(workloadNamespace).Create(ctx, genRouteRole(workloadNamespace), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		plog.G(ctx).Warnf("Could not create route-discovery Role in namespace %q (server route discovery will fall back to CIDR-only): %v", workloadNamespace, err)
		return
	}
	if _, err := clientset.RbacV1().RoleBindings(workloadNamespace).Create(ctx, genRouteRoleBinding(workloadNamespace, managerNamespace), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		plog.G(ctx).Warnf("Could not create route-discovery RoleBinding in namespace %q: %v", workloadNamespace, err)
	}
}

// ensureProxyRBAC creates (idempotently, best-effort) the RBAC that lets the traffic
// manager's ServiceAccount inject/uninject sidecars server-side (ProxyInject/LeaveInject).
// Injection is server-side only (no client fallback), so this grants wildcard resources to
// cover any workload type (incl. CRDs). Scope depends on the manager namespace:
//   - central manager (config.DefaultNamespaceKubevpn): a ClusterRole + ClusterRoleBinding,
//     since a central manager injects across all namespaces (matches the helm chart's
//     ClusterRole by name, so the two are idempotent);
//   - per-namespace manager: a namespaced Role + RoleBinding in the workload namespace.
//
// It never fails the connect: if the RBAC cannot be created (e.g. the user lacks
// permission), ProxyInject later fails with a clear error.
func ensureProxyRBAC(ctx context.Context, clientset kubernetes.Interface, workloadNamespace, managerNamespace string) {
	if workloadNamespace == "" {
		workloadNamespace = managerNamespace
	}
	if managerNamespace == config.DefaultNamespaceKubevpn {
		if _, err := clientset.RbacV1().ClusterRoles().Create(ctx, genProxyClusterRole(), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			plog.G(ctx).Warnf("Could not create proxy-inject ClusterRole (server-side injection may fail): %v", err)
			return
		}
		if _, err := clientset.RbacV1().ClusterRoleBindings().Create(ctx, genProxyClusterRoleBinding(managerNamespace), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			plog.G(ctx).Warnf("Could not create proxy-inject ClusterRoleBinding: %v", err)
		}
		return
	}
	if _, err := clientset.RbacV1().Roles(workloadNamespace).Create(ctx, genProxyRole(workloadNamespace), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		plog.G(ctx).Warnf("Could not create proxy-inject Role in namespace %q (server-side injection may fail): %v", workloadNamespace, err)
		return
	}
	if _, err := clientset.RbacV1().RoleBindings(workloadNamespace).Create(ctx, genProxyRoleBinding(workloadNamespace, managerNamespace), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		plog.G(ctx).Warnf("Could not create proxy-inject RoleBinding in namespace %q: %v", workloadNamespace, err)
	}
}

func cleanupTrafficManagerResources(ctx context.Context, clientset kubernetes.Interface, namespace string) {
	options := metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}
	name := config.ConfigMapPodTrafficManager
	// Route-discovery and proxy-inject RBAC (distinct names). In central mode they live
	// in the workload namespace and are cleaned up there; here we clear the
	// manager-namespace copy (non-central mode, where workload ns == manager ns).
	_ = clientset.RbacV1().RoleBindings(namespace).Delete(ctx, proxyRBACName, options)
	_ = clientset.RbacV1().Roles(namespace).Delete(ctx, proxyRBACName, options)
	_ = clientset.RbacV1().RoleBindings(namespace).Delete(ctx, routeRBACName, options)
	_ = clientset.RbacV1().Roles(namespace).Delete(ctx, routeRBACName, options)
	_ = clientset.RbacV1().RoleBindings(namespace).Delete(ctx, name, options)
	_ = clientset.RbacV1().Roles(namespace).Delete(ctx, name, options)
	_ = clientset.CoreV1().ServiceAccounts(namespace).Delete(ctx, name, options)
	_ = clientset.CoreV1().Services(namespace).Delete(ctx, name, options)
	_ = clientset.CoreV1().ConfigMaps(namespace).Delete(ctx, name, options)
	_ = clientset.CoreV1().Secrets(namespace).Delete(ctx, name, options)
	_ = clientset.BatchV1().Jobs(namespace).Delete(ctx, name, options)
	_ = clientset.AppsV1().Deployments(namespace).Delete(ctx, name, options)
	// Central (kubevpn-namespace) manager owns the cluster-scoped proxy RBAC; remove it too.
	if namespace == config.DefaultNamespaceKubevpn {
		_ = clientset.RbacV1().ClusterRoleBindings().Delete(ctx, proxyRBACName, options)
		_ = clientset.RbacV1().ClusterRoles().Delete(ctx, proxyRBACName, options)
	}
}
