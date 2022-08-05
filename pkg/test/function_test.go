package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/util"
)

var (
	namespace  string
	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config
	cancelFunc context.CancelFunc
)

func TestFunctions(t *testing.T) {
	kubevpnConnect()
	t.Cleanup(cancelFunc)
	t.Parallel()
	t.Run(runtime.FuncForPC(reflect.ValueOf(pingPodIP).Pointer()).Name(), pingPodIP)
	t.Run(runtime.FuncForPC(reflect.ValueOf(dialUDP).Pointer()).Name(), dialUDP)
	t.Run(runtime.FuncForPC(reflect.ValueOf(healthCheckPod).Pointer()).Name(), healthCheckPod)
	t.Run(runtime.FuncForPC(reflect.ValueOf(healthCheckService).Pointer()).Name(), healthCheckService)
	t.Run(runtime.FuncForPC(reflect.ValueOf(shortDomain).Pointer()).Name(), shortDomain)
	t.Run(runtime.FuncForPC(reflect.ValueOf(fullDomain).Pointer()).Name(), fullDomain)
}

func pingPodIP(t *testing.T) {
	ctx, f := context.WithTimeout(context.TODO(), time.Second*60)
	defer f()
	list, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Error(err)
	}
	var wg = &sync.WaitGroup{}
	for _, item := range list.Items {
		if item.Status.Phase == corev1.PodRunning {
			wg.Add(1)
			go func() {
				defer wg.Done()
				command := exec.Command("ping", "-c", "4", item.Status.PodIP)
				if err = command.Run(); err == nil {
					if !command.ProcessState.Success() {
						t.Errorf("can not ping ip: %s of pod: %s", item.Status.PodIP, item.Name)
					}
				}
			}()
		}
	}
	wg.Wait()
}

func healthCheckPod(t *testing.T) {
	podList, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", "productpage").String(),
	})
	if err != nil {
		t.Error(err)
	}
	if len(podList.Items) == 0 {
		t.Error("can not found pods of product page")
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", podList.Items[0].Status.PodIP, podList.Items[0].Spec.Containers[0].Ports[0].ContainerPort)
	req, _ := http.NewRequest("GET", endpoint, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return
	}
	if res == nil || res.StatusCode != 200 {
		t.Errorf("health check not pass")
		return
	}
}

func healthCheckService(t *testing.T) {
	serviceList, err := clientset.CoreV1().Services(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", "productpage").String(),
	})
	if err != nil {
		t.Error(err)
	}
	if len(serviceList.Items) == 0 {
		t.Error("can not found pods of product page")
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", serviceList.Items[0].Spec.ClusterIP, serviceList.Items[0].Spec.Ports[0].Port)
	req, _ := http.NewRequest("GET", endpoint, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return
	}
	if res == nil || res.StatusCode != 200 {
		t.Errorf("health check not pass")
		return
	}
}

func shortDomain(t *testing.T) {
	var app = "productpage"
	serviceList, err := clientset.CoreV1().Services(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", app).String(),
	})
	if err != nil {
		t.Error(err)
	}
	if len(serviceList.Items) == 0 {
		t.Errorf("can not found pods of %s", app)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", app, serviceList.Items[0].Spec.Ports[0].Port)
	req, _ := http.NewRequest("GET", endpoint, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return
	}
	if res == nil || res.StatusCode != 200 {
		t.Errorf("health check not pass")
		return
	}
}

func fullDomain(t *testing.T) {
	var app = "productpage"
	serviceList, err := clientset.CoreV1().Services(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", app).String(),
	})
	if err != nil {
		t.Error(err)
	}
	if len(serviceList.Items) == 0 {
		t.Errorf("can not found pods of %s", app)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", fmt.Sprintf("%s.%s.svc.cluster.local", app, namespace), serviceList.Items[0].Spec.Ports[0].Port)
	req, _ := http.NewRequest("GET", endpoint, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return
	}
	if res == nil || res.StatusCode != 200 {
		t.Errorf("health check not pass")
		return
	}
}

func dialUDP(t *testing.T) {
	port := util.GetAvailableUDPPortOrDie()
	go server(port)

	list, err := clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", "reviews").String(),
	})
	if err != nil {
		t.Error(err)
	}
	var ip string
	for _, item := range list.Items {
		if item.DeletionTimestamp == nil && item.Status.Phase == corev1.PodRunning {
			ip = item.Status.PodIP
			break
		}
	}
	if len(ip) == 0 {
		t.Errorf("can not found pods for service reviews")
	}
	log.Printf("dail udp to ip: %s", ip)
	if err = retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.2, Steps: 5},
		func(err error) bool {
			return err != nil
		}, func() error {
			return client(ip, port)
		}); err != nil {
		t.Errorf("can not access pod ip: %s, port: %v", ip, port)
	}
}

func client(ip string, port int) error {
	udpConn, err := net.DialUDP("udp4", nil, &net.UDPAddr{
		IP:   net.ParseIP(ip),
		Port: port,
	})
	if err != nil {
		fmt.Println("连接失败!", err)
		return err
	}
	defer udpConn.Close()

	err = udpConn.SetDeadline(time.Now().Add(time.Second * 30))
	if err != nil {
		return err
	}

	// 发送数据
	sendData := []byte("hello server!")
	_, err = udpConn.Write(sendData)
	if err != nil {
		fmt.Println("发送数据失败!", err)
		return err
	}

	// 接收数据
	data := make([]byte, 4096)
	read, remoteAddr, err := udpConn.ReadFromUDP(data)
	if err != nil {
		fmt.Println("读取数据失败!", err)
		return err
	}
	fmt.Println(read, remoteAddr)
	fmt.Printf("%s\n", data[0:read])
	return nil
}

func server(port int) {
	// 创建监听
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{
		IP:   net.IPv4(0, 0, 0, 0),
		Port: port,
	})
	if err != nil {
		return
	}
	defer udpConn.Close()

	for {
		data := make([]byte, 4096)
		read, remoteAddr, err := udpConn.ReadFromUDP(data)
		if err != nil {
			fmt.Println("读取数据失败!", err)
			continue
		}
		fmt.Println(read, remoteAddr)
		fmt.Printf("%s\n\n", data[0:read])

		sendData := []byte("hello client!")
		_, err = udpConn.WriteToUDP(sendData, remoteAddr)
		if err != nil {
			fmt.Println("发送数据失败!", err)
			return
		}
	}
}

func kubevpnConnect() {
	var ctx context.Context
	ctx, cancelFunc = context.WithCancel(context.TODO())
	childCtx, timeoutFunc := context.WithTimeout(ctx, 2*time.Hour)

	cmd := exec.CommandContext(ctx, "kubevpn", "connect", "--debug", "--workloads", "deployments/reviews")
	go util.RunWithRollingOutWithChecker(cmd, func(log string) bool {
		ok := strings.Contains(log, "dns service ok")
		if ok {
			timeoutFunc()
		}
		return ok
	})
	<-childCtx.Done()
}

func init() {
	var err error

	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &clientcmd.RecommendedHomeFile
	f := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))

	if config, err = f.ToRESTConfig(); err != nil {
		log.Fatal(err)
	}
	if restclient, err = rest.RESTClientFor(config); err != nil {
		log.Fatal(err)
	}
	if clientset, err = kubernetes.NewForConfig(config); err != nil {
		log.Fatal(err)
	}
	if namespace, _, err = f.ToRawKubeConfigLoader().Namespace(); err != nil {
		log.Fatal(err)
	}
}
