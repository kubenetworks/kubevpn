package handler

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
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/intstr"
	json2 "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

var (
	namespace  string
	clientset  *kubernetes.Clientset
	restconfig *rest.Config
)

func TestFunctions(t *testing.T) {
	Init()
	kubevpnConnect(t)
	t.Run(runtime.FuncForPC(reflect.ValueOf(pingPodIP).Pointer()).Name(), pingPodIP)
	t.Run(runtime.FuncForPC(reflect.ValueOf(dialUDP).Pointer()).Name(), dialUDP)
	t.Run(runtime.FuncForPC(reflect.ValueOf(healthCheckPod).Pointer()).Name(), healthCheckPod)
	t.Run(runtime.FuncForPC(reflect.ValueOf(healthCheckService).Pointer()).Name(), healthCheckService)
	t.Run(runtime.FuncForPC(reflect.ValueOf(shortDomain).Pointer()).Name(), shortDomain)
	t.Run(runtime.FuncForPC(reflect.ValueOf(fullDomain).Pointer()).Name(), fullDomain)
}

func pingPodIP(t *testing.T) {
	list, err := clientset.CoreV1().Pods(namespace).List(context.Background(), v1.ListOptions{})
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
						t.Errorf("Failed to ping IP: %s of pod: %s", item.Status.PodIP, item.Name)
					}
				}
			}()
		}
	}
	wg.Wait()
}

func healthCheckPod(t *testing.T) {
	var app = "authors"
	podList, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), v1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", app).String(),
	})
	if err != nil {
		t.Error(err)
	}
	if len(podList.Items) == 0 {
		t.Error("Failed to found pods of authors")
	}
	for _, pod := range podList.Items {
		pod := pod
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		endpoint := fmt.Sprintf("http://%s:%v/health", pod.Status.PodIP, pod.Spec.Containers[0].Ports[0].ContainerPort)
		req, _ := http.NewRequest("GET", endpoint, nil)
		var res *http.Response
		err = retry.OnError(
			wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.2, Steps: 5},
			func(err error) bool {
				return err != nil
			},
			func() error {
				res, err = http.DefaultClient.Do(req)
				return err
			},
		)
		if err != nil {
			t.Error(err)
		}
		if res == nil || res.StatusCode != 200 {
			t.Errorf("health check not pass")
		}
	}
}

func healthCheckService(t *testing.T) {
	var app = "authors"
	serviceList, err := clientset.CoreV1().Services(namespace).List(context.TODO(), v1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", app).String(),
	})
	if err != nil {
		t.Error(err)
	}
	if len(serviceList.Items) == 0 {
		t.Error("Failed to found pods of authors")
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", serviceList.Items[0].Spec.ClusterIP, serviceList.Items[0].Spec.Ports[0].Port)
	req, _ := http.NewRequest("GET", endpoint, nil)
	var res *http.Response
	err = retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.2, Steps: 5},
		func(err error) bool {
			return err != nil
		},
		func() error {
			res, err = http.DefaultClient.Do(req)
			return err
		},
	)
	if err != nil {
		t.Error(err)
	}
	if res == nil || res.StatusCode != 200 {
		t.Errorf("health check not pass")
		return
	}
}

func shortDomain(t *testing.T) {
	var app = "authors"
	serviceList, err := clientset.CoreV1().Services(namespace).List(context.TODO(), v1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", app).String(),
	})
	if err != nil {
		t.Error(err)
	}
	if len(serviceList.Items) == 0 {
		t.Errorf("Failed to found pods of %s", app)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", app, serviceList.Items[0].Spec.Ports[0].Port)
	req, _ := http.NewRequest("GET", endpoint, nil)
	var res *http.Response
	err = retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.2, Steps: 5},
		func(err error) bool {
			return err != nil
		},
		func() error {
			res, err = http.DefaultClient.Do(req)
			return err
		},
	)
	if err != nil {
		t.Error(err)
	}
	if res == nil || res.StatusCode != 200 {
		t.Errorf("health check not pass")
	}
}

func fullDomain(t *testing.T) {
	var app = "authors"
	serviceList, err := clientset.CoreV1().Services(namespace).List(context.TODO(), v1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", app).String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(serviceList.Items) == 0 {
		t.Fatalf("Failed to found pods of %s", app)
	}

	domains := []string{
		fmt.Sprintf("%s.%s.svc.cluster.local", app, namespace),
		fmt.Sprintf("%s.%s.svc", app, namespace),
		fmt.Sprintf("%s.%s", app, namespace),
	}

	for _, domain := range domains {
		port := serviceList.Items[0].Spec.Ports[0].Port
		endpoint := fmt.Sprintf("http://%s:%v/health", domain, port)
		var req *http.Request
		req, err = http.NewRequest("GET", endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		var res *http.Response
		err = retry.OnError(
			wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.2, Steps: 5},
			func(err error) bool {
				return err != nil
			},
			func() error {
				res, err = http.DefaultClient.Do(req)
				return err
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if res == nil || res.StatusCode != 200 {
			t.Fatal("health check not pass")
		}
	}
}

func dialUDP(t *testing.T) {
	port, _ := util.GetAvailableUDPPortOrDie()
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
		t.Errorf("Failed to found pods for service reviews")
		return
	}
	log.Printf("Dail udp to IP: %s", ip)
	if err = retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.2, Steps: 5},
		func(err error) bool {
			return err != nil
		}, func() error {
			return udpclient(ip, port)
		}); err != nil {
		t.Errorf("Failed to access pod IP: %s, port: %v", ip, port)
	}
}

func udpclient(ip string, port int) error {
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

func kubevpnConnect(t *testing.T) {
	cmd := exec.Command("kubevpn", "proxy", "--debug", "deployments/reviews")
	check := func(log string) {
		line := "+" + strings.Repeat("-", len(log)-2) + "+"
		t.Log(line)
		t.Log(log)
		t.Log(line)
		t.Log("\n")
	}
	stdout, stderr, err := util.RunWithRollingOutWithChecker(cmd, check)
	if err != nil {
		t.Log(stdout, stderr)
		t.Error(err)
		t.Fail()
		return
	}
}

func Init() {
	var err error

	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &clientcmd.RecommendedHomeFile
	f := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))

	if restconfig, err = f.ToRESTConfig(); err != nil {
		log.Fatal(err)
	}
	if clientset, err = kubernetes.NewForConfig(restconfig); err != nil {
		log.Fatal(err)
	}
	if namespace, _, err = f.ToRawKubeConfigLoader().Namespace(); err != nil {
		log.Fatal(err)
	}
}

func TestWaitBackoff(t *testing.T) {
	var last = time.Now()
	_ = retry.OnError(
		wait.Backoff{
			Steps:    10,
			Duration: time.Millisecond * 50,
		}, func(err error) bool {
			return err != nil
		}, func() error {
			now := time.Now()
			fmt.Println(now.Sub(last).String())
			last = now
			return fmt.Errorf("")
		})
}

func TestArray(t *testing.T) {
	s := []int{1, 2, 3, 1, 2, 3, 1, 2, 3}
	for i := 0; i < 3; i++ {
		ints := s[i*3 : i*3+3]
		println(ints[0], ints[1], ints[2])
	}
}
func TestPatch(t *testing.T) {
	var p = corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path:   "/health",
			Port:   intstr.FromInt32(9080),
			Scheme: "HTTP",
		}},
	}
	marshal, err := json2.Marshal(p)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(marshal))

	var pp corev1.Probe
	err = json2.Unmarshal(marshal, &pp)
	if err != nil {
		panic(err)
	}
	bytes, err := yaml.Marshal(pp)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(bytes))
}
