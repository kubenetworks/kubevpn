package handler

import (
	"context"
	"crypto/md5"
	"fmt"
	"net"
	"os/exec"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	net2 "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/util"
)

var (
	clientConfig = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: clientcmd.RecommendedHomeFile}, nil,
	)
	clientconfig, _  = clientConfig.ClientConfig()
	clientsets, _    = kubernetes.NewForConfig(clientconfig)
	namespaces, _, _ = clientConfig.Namespace()
)

func TestGetCIDR(t *testing.T) {
	cidr, err := getCIDR(clientsets, namespaces)
	if err == nil {
		for _, ipNet := range cidr {
			fmt.Println(ipNet)
		}
	}
}

func TestPingUsingCommand(t *testing.T) {
	list, _ := clientsets.CoreV1().Services(namespaces).List(context.Background(), metav1.ListOptions{})
	for _, service := range list.Items {
		for _, clusterIP := range service.Spec.ClusterIPs {
			_ = exec.Command("ping", clusterIP, "-c", "4").Run()
		}
	}
}

func TestGetMacAddress(t *testing.T) {
	interfaces, _ := net.Interfaces()
	hostInterface, _ := net2.ChooseHostInterface()
	for _, i := range interfaces {
		//fmt.Printf("%s -> %s\n", i.Name, i.HardwareAddr.String())
		addrs, _ := i.Addrs()
		for _, addr := range addrs {
			if hostInterface.Equal(addr.(*net.IPNet).IP) {
				hash := md5.New()
				hash.Write([]byte(i.HardwareAddr.String()))
				sum := hash.Sum(nil)
				toInt := util.BytesToInt(sum)
				fmt.Println(toInt)
			}
		}
	}
}

func TestPingUsingCode(t *testing.T) {
	conn, err := net.DialTimeout("ip4:icmp", "www.baidu.com", time.Second*5)
	if err != nil {
		log.Print(err)
		return
	}
	var msg [512]byte
	msg[0] = 8
	msg[1] = 0
	msg[2] = 0
	msg[3] = 0
	msg[4] = 0
	msg[5] = 13
	msg[6] = 0
	msg[7] = 37

	length := 8
	check := checkSum(msg[0:length])
	msg[2] = byte(check >> 8)
	msg[3] = byte(check & 255)
	_, err = conn.Write(msg[0:length])
	if err != nil {
		log.Print(err)
		return
	}
	conn.Read(msg[0:])
	log.Println(msg[5] == 13)
	log.Println(msg[7] == 37)
}

func checkSum(msg []byte) uint16 {
	sum := 0
	for n := 1; n < len(msg)-1; n += 2 {
		sum += int(msg[n])*256 + int(msg[n+1])
	}
	sum = (sum >> 16) + (sum & 0xffff)
	sum += sum >> 16
	return uint16(^sum)
}

func TestPatchAnnotation(t *testing.T) {
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = &clientcmd.RecommendedHomeFile
	factory := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))
	do := factory.NewBuilder().
		Unstructured().
		NamespaceParam("default").DefaultNamespace().AllNamespaces(false).
		ResourceTypeOrNameArgs(true, "deployments/reviews").
		ContinueOnError().
		Latest().
		Flatten().
		TransformRequests(func(req *rest.Request) { req.Param("includeObject", "Object") }).
		Do()
	err := do.Err()
	if err != nil {
		panic(err)
	}
	infos, err := do.Infos()
	if err != nil {
		panic(err)
	}
	info := infos[0]
	helper := resource.NewHelper(info.Client, info.Mapping)

	object, err := helper.Patch(
		info.Namespace,
		info.Name,
		types.JSONPatchType,
		[]byte(`[{"op":"replace","path":"/metadata/annotations/dev.nocalhost","value":{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"annotations":{"deployment.kubernetes.io/revision":"1","dev.nocalhost/application-name":"bookinfo","dev.nocalhost/application-namespace":"default"},"labels":{"app":"reviews","app.kubernetes.io/managed-by":"nocalhost"},"name":"reviews","namespace":"default","selfLink":"/apis/apps/v1/namespaces/default/deployments/reviews"},"spec":{"progressDeadlineSeconds":600,"replicas":1,"revisionHistoryLimit":10,"selector":{"matchLabels":{"app":"reviews"}},"strategy":{"rollingUpdate":{"maxSurge":"25%","maxUnavailable":"25%"},"type":"RollingUpdate"},"template":{"metadata":{"creationTimestamp":null,"labels":{"app":"reviews"}},"spec":{"containers":[{"env":[{"name":"LOG_DIR","value":"/tmp/logs"}],"image":"nocalhost-docker.pkg.coding.net/nocalhost/bookinfo/reviews:latest","imagePullPolicy":"Always","name":"reviews","ports":[{"containerPort":9080,"protocol":"TCP"}],"readinessProbe":{"failureThreshold":3,"initialDelaySeconds":5,"periodSeconds":10,"successThreshold":1,"tcpSocket":{"port":9080},"timeoutSeconds":1},"resources":{"limits":{"cpu":"1","memory":"512Mi"},"requests":{"cpu":"10m","memory":"32Mi"}},"terminationMessagePath":"/dev/termination-log","terminationMessagePolicy":"File","volumeMounts":[{"mountPath":"/tmp","name":"tmp"},{"mountPath":"/opt/ibm/wlp/output","name":"wlp-output"}]}],"dnsPolicy":"ClusterFirst","restartPolicy":"Always","schedulerName":"default-scheduler","securityContext":{},"terminationGracePeriodSeconds":30,"volumes":[{"emptyDir":{},"name":"wlp-output"},{"emptyDir":{},"name":"tmp"}]}}}}}]`),
		&metav1.PatchOptions{})
	if err != nil {
		panic(err)
	}
	fmt.Println(object.(*unstructured.Unstructured).GetAnnotations())
}
