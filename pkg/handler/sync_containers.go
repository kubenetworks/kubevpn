package handler

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	libconfig "github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/netutil"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/syncthing"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func genSyncthingContainer(remoteDir string, syncDataDirName string, image string) *v1.Container {
	return &v1.Container{
		Name:  config.ContainerSidecarSyncthing,
		Image: image,
		Command: []string{
			"kubevpn",
			"syncthing",
			"--dir",
			remoteDir,
		},
		Resources: v1.ResourceRequirements{
			Requests: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("200m"),
				v1.ResourceMemory: resource.MustParse("512Mi"),
			},
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("1000m"),
				v1.ResourceMemory: resource.MustParse("1024Mi"),
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      syncDataDirName,
				ReadOnly:  false,
				MountPath: remoteDir,
			},
		},
		SecurityContext: &v1.SecurityContext{
			RunAsUser:  ptr.To[int64](0),
			RunAsGroup: ptr.To[int64](0),
		},
	}
}

func genVPNContainer(workload string, namespace string, image string, args []string) *v1.Container {
	return &v1.Container{
		Name:  config.ContainerSidecarVPN,
		Image: image,
		Command: append([]string{
			"kubevpn",
			"proxy",
			workload,
			"--kubeconfig", "/tmp/.kube/" + config.KUBECONFIG,
			"--namespace", namespace,
			"--image", image,
			"--foreground",
		}, args...),
		Env: []v1.EnvVar{},
		Resources: v1.ResourceRequirements{
			Requests: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("200m"),
				v1.ResourceMemory: resource.MustParse("512Mi"),
			},
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("1000m"),
				v1.ResourceMemory: resource.MustParse("2048Mi"),
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      config.KUBECONFIG,
				ReadOnly:  false,
				MountPath: "/tmp/.kube",
			},
		},
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{
				Exec: &v1.ExecAction{
					Command: []string{
						"/bin/bash",
						"-c",
						`
echo 1 > /proc/sys/net/ipv4/ip_forward
echo 0 > /proc/sys/net/ipv6/conf/all/disable_ipv6
echo 1 > /proc/sys/net/ipv6/conf/all/forwarding
echo 1 > /proc/sys/net/ipv4/conf/all/route_localnet
update-alternatives --set iptables /usr/sbin/iptables-legacy`,
					},
				},
			},
		},
		ImagePullPolicy: v1.PullIfNotPresent,
		SecurityContext: &v1.SecurityContext{
			Capabilities: &v1.Capabilities{
				Add: []v1.Capability{
					"NET_ADMIN",
				},
			},
			RunAsUser:  ptr.To[int64](0),
			RunAsGroup: ptr.To[int64](0),
			Privileged: ptr.To(true),
		},
	}
}

func clearObjectMetadata(u *unstructured.Unstructured) {
	if u == nil {
		return
	}

	delete(u.Object, "status")
	_ = unstructured.SetNestedField(u.Object, nil, "status")

	u.SetManagedFields(nil)
	u.SetResourceVersion("")
	u.SetCreationTimestamp(metav1.NewTime(time.Time{}))
	u.SetUID("")
	u.SetGeneration(0)
	a := u.GetAnnotations()
	if len(a) == 0 {
		a = map[string]string{}
	}
	delete(a, "kubectl.kubernetes.io/last-applied-configuration")
	u.SetAnnotations(a)
}

func (d *SyncOptions) SyncDir(ctx context.Context, labels string) error {
	list, err := util.GetRunningPodList(ctx, d.clientset, d.Namespace, labels)
	if err != nil {
		return err
	}
	remoteAddr := net.JoinHostPort(list[0].Status.PodIP, strconv.Itoa(libconfig.DefaultTCPPort))
	localPort, _ := util.GetAvailableTCPPort()
	localAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	err = syncthing.StartClient(d.ctx, d.LocalDir, localAddr, remoteAddr)
	if err != nil {
		return err
	}
	d.syncthingGUIAddr = (&url.URL{Scheme: "http", Host: localAddr}).String()
	plog.G(ctx).Infof("Access the syncthing GUI via the following URL: %s", d.syncthingGUIAddr)
	go func() {
		client := syncthing.NewClient(localAddr)
		podName := list[0].Name
		for d.ctx.Err() == nil {
			func() {
				defer time.Sleep(time.Second * 2)

				sortBy := activePodsSortFunc
				_, _, _ = polymorphichelpers.GetFirstPod(d.clientset.CoreV1(), d.Namespace, labels, time.Second*30, sortBy)
				list, err := util.GetRunningPodList(d.ctx, d.clientset, d.Namespace, labels)
				if err != nil {
					plog.G(ctx).Error(err)
					return
				}
				if podName == list[0].Name {
					return
				}

				podName = list[0].Name
				plog.G(ctx).Debugf("Detect newer pod %s", podName)
				var conf *libconfig.Configuration
				conf, err = client.GetConfig(d.ctx)
				if err != nil {
					plog.G(ctx).Errorf("Failed to get config from syncthing: %v", err)
					return
				}
				for i := range conf.Devices {
					if config.RemoteDeviceID.Equals(conf.Devices[i].DeviceID) {
						addr := netutil.AddressURL("tcp", net.JoinHostPort(list[0].Status.PodIP, strconv.Itoa(libconfig.DefaultTCPPort)))
						conf.Devices[i].Addresses = []string{addr}
						plog.G(ctx).Debugf("Use newer remote syncthing endpoint: %s", addr)
					}
				}
				err = client.PutConfig(d.ctx, conf)
				if err != nil {
					plog.G(ctx).Errorf("Failed to set config to syncthing: %v", err)
				}
			}()
		}
	}()
	return nil
}

func (d *SyncOptions) ConvertApiServerToNodeIP(ctx context.Context, kubeconfigBytes []byte) ([]byte, error) {
	list, err := d.clientset.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{Limit: 100})
	if err != nil {
		return nil, err
	}
	var result string
	for _, item := range list.Items {
		result, err = util.Shell(ctx, d.clientset, d.config, item.Name, "", d.Namespace, []string{"env"})
		if err == nil {
			break
		}
	}
	parse, err := godotenv.Parse(strings.NewReader(result))
	if err != nil {
		return nil, err
	}

	host := parse["KUBERNETES_SERVICE_HOST"]
	port := parse["KUBERNETES_SERVICE_PORT"]

	addrPort, err := netip.ParseAddrPort(net.JoinHostPort(host, port))
	if err != nil {
		return nil, err
	}

	var newKubeconfigBytes []byte
	newKubeconfigBytes, _, err = util.ModifyAPIServer(ctx, kubeconfigBytes, addrPort)
	if err != nil {
		return nil, err
	}
	return newKubeconfigBytes, nil
}
