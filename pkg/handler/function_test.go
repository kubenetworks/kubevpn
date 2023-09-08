package handler

import (
	"context"
	"encoding/json"
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
	restconfig *rest.Config
)

func TestFunctions(t *testing.T) {
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
						t.Errorf("can not ping ip: %s of pod: %s", item.Status.PodIP, item.Name)
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
		t.Error("can not found pods of authors")
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
		t.Error("can not found pods of authors")
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
		t.Errorf("can not found pods of %s", app)
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
		t.Error(err)
	}
	if len(serviceList.Items) == 0 {
		t.Errorf("can not found pods of %s", app)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", fmt.Sprintf("%s.%s.svc.cluster.local", app, namespace), serviceList.Items[0].Spec.Ports[0].Port)
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
		return
	}
	log.Printf("dail udp to ip: %s", ip)
	if err = retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.2, Steps: 5},
		func(err error) bool {
			return err != nil
		}, func() error {
			return udpclient(ip, port)
		}); err != nil {
		t.Errorf("can not access pod ip: %s, port: %v", ip, port)
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
	ctx2, timeoutFunc := context.WithTimeout(context.Background(), 2*time.Hour)

	cmd := exec.Command("kubevpn", "proxy", "--debug", "deployments/reviews")
	go func() {
		stdout, stderr, err := util.RunWithRollingOutWithChecker(cmd, func(log string) {
			ok := strings.Contains(log, "dns service ok")
			if ok {
				timeoutFunc()
			}
		})
		defer timeoutFunc()
		if err != nil {
			t.Log(stdout, stderr)
			t.Error(err)
			t.Fail()
			return
		}
	}()
	<-ctx2.Done()
}

func init1() {
	var err error

	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &clientcmd.RecommendedHomeFile
	f := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))

	if restconfig, err = f.ToRESTConfig(); err != nil {
		log.Fatal(err)
	}
	if restclient, err = rest.RESTClientFor(restconfig); err != nil {
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
	s := "W3sib3AiOiJyZXBsYWNlIiwicGF0aCI6Ii9zcGVjL3RlbXBsYXRlL3NwZWMvY29udGFpbmVycy8wL3JlYWRpbmVzc1Byb2JlIiwidmFsdWUiOm51bGx9LHsib3AiOiJyZXBsYWNlIiwicGF0aCI6Ii9zcGVjL3RlbXBsYXRlL3NwZWMvY29udGFpbmVycy8wL2xpdmVuZXNzUHJvYmUiLCJ2YWx1ZSI6eyJodHRwR2V0Ijp7InBhdGgiOiIvaGVhbHRoIiwicG9ydCI6OTA4MCwic2NoZW1lIjoiSFRUUCJ9LCJ0aW1lb3V0U2Vjb25kcyI6MSwicGVyaW9kU2Vjb25kcyI6MTAsInN1Y2Nlc3NUaHJlc2hvbGQiOjEsImZhaWx1cmVUaHJlc2hvbGQiOjN9fSx7Im9wIjoicmVwbGFjZSIsInBhdGgiOiIvc3BlYy90ZW1wbGF0ZS9zcGVjL2NvbnRhaW5lcnMvMC9zdGFydHVwUHJvYmUiLCJ2YWx1ZSI6bnVsbH0seyJvcCI6InJlcGxhY2UiLCJwYXRoIjoiL3NwZWMvdGVtcGxhdGUvc3BlYy9jb250YWluZXJzLzEvcmVhZGluZXNzUHJvYmUiLCJ2YWx1ZSI6bnVsbH0seyJvcCI6InJlcGxhY2UiLCJwYXRoIjoiL3NwZWMvdGVtcGxhdGUvc3BlYy9jb250YWluZXJzLzEvbGl2ZW5lc3NQcm9iZSIsInZhbHVlIjpudWxsfSx7Im9wIjoicmVwbGFjZSIsInBhdGgiOiIvc3BlYy90ZW1wbGF0ZS9zcGVjL2NvbnRhaW5lcnMvMS9zdGFydHVwUHJvYmUiLCJ2YWx1ZSI6bnVsbH1d"
	var pp []P
	err := json.Unmarshal([]byte(s), &pp)
	if err != nil {
		panic(err)
	}
	fmt.Println(pp)
}
