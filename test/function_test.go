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
	t.Parallel()
	t.Run(runtime.FuncForPC(reflect.ValueOf(pingPodIP).Pointer()).Name(), pingPodIP)
	t.Run(runtime.FuncForPC(reflect.ValueOf(curlUDP).Pointer()).Name(), curlUDP)
	t.Run(runtime.FuncForPC(reflect.ValueOf(healthCheck).Pointer()).Name(), healthCheck)
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

func healthCheck(t *testing.T) {
	list, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), v1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", "productpage").String(),
	})
	if err != nil {
		t.Error(err)
	}
	if len(list.Items) == 0 {
		t.Error("can not found pods of product page")
	}
	endpoint := fmt.Sprintf("http://%s/health", list.Items[0].Status.PodIP)
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

func curlUDP(t *testing.T) {
	list, err := clientset.CoreV1().Pods(namespace).List(context.Background(), v1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", "reviews").String(),
	})
	if err != nil {
		t.Error(err)
	}
	if len(list.Items) == 0 {
		t.Errorf("can not found pods for service reviews")
	}
	port := util.GetAvailableUDPPortOrDie()
	go server(port)
	time.Sleep(time.Second * 2)
	err = client(list.Items[0].Status.PodIP, port)
	if err != nil {
		t.Error(err)
	}
}

func client(ip string, port int) error {
	socket, err := net.DialUDP("udp4", nil, &net.UDPAddr{
		IP:   net.ParseIP(ip),
		Port: port,
	})
	if err != nil {
		fmt.Println("连接失败!", err)
		return err
	}
	defer socket.Close()

	// 发送数据
	senddata := []byte("hello server!")
	_, err = socket.Write(senddata)
	if err != nil {
		fmt.Println("发送数据失败!", err)
		return err
	}

	// 接收数据
	data := make([]byte, 4096)
	read, remoteAddr, err := socket.ReadFromUDP(data)
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
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{
		IP:   net.IPv4(0, 0, 0, 0),
		Port: port,
	})
	if err != nil {
		return
	}
	defer socket.Close()

	for {
		data := make([]byte, 4096)
		read, remoteAddr, err := socket.ReadFromUDP(data)
		if err != nil {
			fmt.Println("读取数据失败!", err)
			continue
		}
		fmt.Println(read, remoteAddr)
		fmt.Printf("%s\n\n", data[0:read])

		senddata := []byte("hello client!")
		_, err = socket.WriteToUDP(senddata, remoteAddr)
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

	command := exec.CommandContext(ctx, "kubevpn", "connect", "--workloads", "deployments/reviews-v1")
	go util.RunWithRollingOutWithChecker(command, func(log string) bool {
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
