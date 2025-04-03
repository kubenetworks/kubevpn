package handler

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
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
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

var (
	namespace  string
	clientset  *kubernetes.Clientset
	restconfig *rest.Config
)

const (
	local  = `{"status": "Reviews is healthy on local pc"}`
	remote = `{"status": "Reviews is healthy"}`
)

func TestFunctions(t *testing.T) {
	// 1) test connect
	Init()
	t.Run("kubevpnConnect", kubevpnConnect)
	t.Run("commonTest", commonTest)

	// 2) test proxy mode
	t.Run("kubevpnProxy", kubevpnProxy)
	t.Run("commonTest", commonTest)
	t.Run("testUDP", testUDP)
	t.Run("proxyServiceReviewsServiceIP", proxyServiceReviewsServiceIP)
	t.Run("proxyServiceReviewsPodIP", proxyServiceReviewsPodIP)

	// 3) test proxy mode with service mesh
	t.Run("kubevpnLeave", kubevpnLeave)
	t.Run("kubevpnProxyWithServiceMesh", kubevpnProxyWithServiceMesh)
	t.Run("commonTest", commonTest)
	t.Run("serviceMeshReviewsServiceIP", serviceMeshReviewsServiceIP)
	t.Run("serviceMeshReviewsPodIP", serviceMeshReviewsPodIP)

	// 4) test proxy mode with service mesh and gvisor
	t.Run("kubevpnLeave", kubevpnLeave)
	t.Run("kubevpnUninstall", kubevpnUninstall)
	t.Run("kubevpnProxyWithServiceMeshAndGvisorMode", kubevpnProxyWithServiceMeshAndGvisorMode)
	t.Run("commonTest", commonTest)
	t.Run("serviceMeshReviewsServiceIP", serviceMeshReviewsServiceIP)
	t.Run("kubevpnQuit", kubevpnQuit)
}

func commonTest(t *testing.T) {
	// 1) test domain access
	t.Run("kubevpnStatus", kubevpnStatus)
	t.Run("pingPodIP", pingPodIP)
	t.Run("healthCheckPodDetails", healthCheckPodDetails)
	t.Run("healthCheckServiceDetails", healthCheckServiceDetails)
	t.Run("shortDomainDetails", shortDomainDetails)
	t.Run("fullDomainDetails", fullDomainDetails)
}

func pingPodIP(t *testing.T) {
	list, err := clientset.CoreV1().Pods(namespace).List(context.Background(), v1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var wg = &sync.WaitGroup{}
	for _, item := range list.Items {
		if item.Status.Phase != corev1.PodRunning {
			continue
		}
		if item.DeletionTimestamp != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				cmd := exec.Command("ping", "-c", "1", item.Status.PodIP)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				err = cmd.Run()
				if err == nil && cmd.ProcessState.Success() {
					return
				}
			}
			t.Errorf("Failed to ping IP: %s of pod: %s", item.Status.PodIP, item.Name)
			kubectl(t)
		}()
	}
	wg.Wait()
}

func healthCheckPodDetails(t *testing.T) {
	var app = "details"
	ip, err := getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	healthChecker(t, endpoint, nil, "")
}

func healthChecker(t *testing.T, endpoint string, header map[string]string, keyword string) {
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Add(k, v)
	}

	err = retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 1, Jitter: 0, Steps: 120},
		func(err error) bool { return err != nil },
		func() error {
			var resp *http.Response
			resp, err = (&http.Client{Timeout: time.Second * 5}).Do(req)
			if err != nil {
				t.Logf("failed to do health check endpoint: %s: %v", endpoint, err)
				return err
			}
			if resp.StatusCode != 200 {
				if resp.Body != nil {
					defer resp.Body.Close()
					all, _ := io.ReadAll(resp.Body)
					return fmt.Errorf("status code is %s, conetent: %v", resp.Status, string(all))
				}
				return fmt.Errorf("status code is %s", resp.Status)
			}
			defer resp.Body.Close()
			if keyword != "" {
				content, err := io.ReadAll(resp.Body)
				if err != nil {
					return err
				}
				if string(content) != keyword {
					return fmt.Errorf("response=%s, want: %s", string(content), keyword)
				}
				return nil
			}
			return nil
		},
	)
	if err != nil {
		kubectl(t)
		t.Fatal(err)
	}
}

func healthCheckServiceDetails(t *testing.T) {
	var app = "details"
	ip, err := getServiceIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	healthChecker(t, endpoint, nil, "")
}

func shortDomainDetails(t *testing.T) {
	var app = "details"
	endpoint := fmt.Sprintf("http://%s:%v/health", app, 9080)
	healthChecker(t, endpoint, nil, "")
}

func fullDomainDetails(t *testing.T) {
	var app = "details"
	domains := []string{
		fmt.Sprintf("%s.%s.svc.cluster.local", app, namespace),
		fmt.Sprintf("%s.%s.svc", app, namespace),
		fmt.Sprintf("%s.%s", app, namespace),
	}

	for _, domain := range domains {
		endpoint := fmt.Sprintf("http://%s:%v/health", domain, 9080)
		healthChecker(t, endpoint, nil, "")
	}
}

func serviceMeshReviewsPodIP(t *testing.T) {
	app := "reviews"
	ip, err := getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	healthChecker(t, endpoint, nil, remote)
	healthChecker(t, endpoint, map[string]string{"env": "test"}, local)
}

func serviceMeshReviewsServiceIP(t *testing.T) {
	app := "reviews"
	ip, err := getServiceIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	healthChecker(t, endpoint, nil, remote)
	healthChecker(t, endpoint, map[string]string{"env": "test"}, local)
}

func getServiceIP(app string) (string, error) {
	serviceList, err := clientset.CoreV1().Services(namespace).List(context.Background(), v1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", app).String(),
	})
	if err != nil {
		return "", err
	}
	var ip string
	for _, item := range serviceList.Items {
		ip = item.Spec.ClusterIP
		if ip != "" {
			return ip, nil
		}
	}
	return "", fmt.Errorf("failed to found service ip for service %s", app)
}

func proxyServiceReviewsPodIP(t *testing.T) {
	app := "reviews"
	ip, err := getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	healthChecker(t, endpoint, nil, local)
	healthChecker(t, endpoint, map[string]string{"env": "test"}, local)
}

func getPodIP(app string) (string, error) {
	list, err := clientset.CoreV1().Pods(namespace).List(context.Background(), v1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", app).String(),
	})
	if err != nil {
		return "", err
	}
	for _, pod := range list.Items {
		if pod.DeletionTimestamp == nil &&
			pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			return pod.Status.PodIP, nil
		}
	}
	return "", fmt.Errorf("failed to found pod ip for service %s", app)
}

func proxyServiceReviewsServiceIP(t *testing.T) {
	app := "reviews"
	ip, err := getServiceIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	healthChecker(t, endpoint, nil, local)
	healthChecker(t, endpoint, map[string]string{"env": "test"}, local)
}

func testUDP(t *testing.T) {
	app := "reviews"
	port, _ := util.GetAvailableUDPPortOrDie()
	go udpServer(port)

	ip, err := getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	log.Printf("Dail udp to IP: %s", ip)
	err = retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.2, Steps: 5},
		func(err error) bool {
			return err != nil
		},
		func() error {
			return udpClient(ip, port)
		})
	if err != nil {
		t.Fatalf("Failed to access pod IP: %s, port: %v", ip, port)
	}
}

func udpClient(ip string, port int) error {
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

func udpServer(port int) {
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
	cmd := exec.Command("kubevpn", "connect", "--debug")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func kubevpnProxy(t *testing.T) {
	cmd := exec.Command("kubevpn", "proxy", "deployments/reviews", "--debug")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func kubevpnProxyWithServiceMesh(t *testing.T) {
	cmd := exec.Command("kubevpn", "proxy", "deployments/reviews", "--headers", "env=test", "--debug")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func kubevpnProxyWithServiceMeshAndGvisorMode(t *testing.T) {
	cmd := exec.Command("kubevpn", "proxy", "deployments/reviews", "--headers", "env=test", "--netstack", "gvisor", "--debug")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func kubevpnLeave(t *testing.T) {
	cmd := exec.Command("kubevpn", "leave", "deployments/reviews")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func kubevpnUninstall(t *testing.T) {
	cmd := exec.Command("kubevpn", "uninstall", "kubevpn")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func kubevpnStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func kubevpnQuit(t *testing.T) {
	cmd := exec.Command("kubevpn", "quit")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func kubectl(t *testing.T) {
	cmd := exec.Command("kubectl", "get", "pods", "-o", "wide")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command("kubectl", "get", "services", "-o", "wide")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func Init() {
	var err error

	configFlags := genericclioptions.NewConfigFlags(true)
	f := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))

	if restconfig, err = f.ToRESTConfig(); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
	if clientset, err = kubernetes.NewForConfig(restconfig); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
	if namespace, _, err = f.ToRawKubeConfigLoader().Namespace(); err != nil {
		plog.G(context.Background()).Fatal(err)
	}

	go startupHttpServer(local)
}

func startupHttpServer(str string) {
	var health = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(str))
	}

	http.HandleFunc("/", health)
	http.HandleFunc("/health", health)
	log.Println("Start listening http port 9080 ...")
	if err := http.ListenAndServe(":9080", nil); err != nil {
		panic(err)
	}
}
