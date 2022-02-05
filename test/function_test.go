package test

import (
	"context"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/util"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"net"
	"net/http"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	namespace  string
	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config
	cancelFunc context.CancelFunc
)

func TestFunctions(t *testing.T) {
	t.Cleanup(cancelFunc)
	//t.Parallel()
	t.Run(runtime.FuncForPC(reflect.ValueOf(pingPodIP).Pointer()).Name(), pingPodIP)
	//t.Run(runtime.FuncForPC(reflect.ValueOf(dialUDP).Pointer()).Name(), dialUDP)
	t.Run(runtime.FuncForPC(reflect.ValueOf(healthCheckPod).Pointer()).Name(), healthCheckPod)
	t.Run(runtime.FuncForPC(reflect.ValueOf(healthCheckService).Pointer()).Name(), healthCheckService)
}

func pingPodIP(t *testing.T) {
	ctx, f := context.WithTimeout(context.TODO(), time.Second*60)
	defer f()
	list, err := clientset.CoreV1().Pods(namespace).List(ctx, v1.ListOptions{})
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
	podList, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), v1.ListOptions{
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
	serviceList, err := clientset.CoreV1().Services(namespace).List(context.TODO(), v1.ListOptions{
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

func dialUDP(t *testing.T) {
	port := util.GetAvailableUDPPortOrDie()
	go server(port)

	list, err := clientset.CoreV1().Pods(namespace).List(context.Background(), v1.ListOptions{
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
	time.Sleep(time.Second * 5)
	log.Infof("dail udp to ip: %s", ip)
	for i := 0; i < 10; i++ {
		err = client(ip, port)
		if err == nil {
			return
		}
		time.Sleep(time.Second * 2)
	}
	t.Errorf("can not access pod ip: %s", ip)
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

func init() {
	initClient()
	var ctx context.Context
	ctx, cancelFunc = context.WithCancel(context.TODO())
	timeoutCtx, timeoutFunc := context.WithTimeout(ctx, time.Minute*10)

	cmd := exec.CommandContext(ctx, "kubevpn", "connect", "--workloads", "deployments/reviews")
	go util.RunWithRollingOutWithChecker(cmd, func(log string) bool {
		ok := strings.Contains(log, "dns service ok")
		if ok {
			timeoutFunc()
		}
		return ok
	})
	<-timeoutCtx.Done()
}

func initClient() {
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
