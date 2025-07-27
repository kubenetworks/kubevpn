package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	pkgconfig "github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

var (
	namespace  string
	clientset  *kubernetes.Clientset
	restconfig *rest.Config
)

const (
	local     = `{"status": "Reviews is healthy on local pc"}`
	local8080 = `{"status": "Reviews is healthy on local pc 8080"}`
	remote    = `{"status": "Reviews is healthy"}`
)

func TestFunctions(t *testing.T) {
	// 1) test connect
	t.Run("init", Init)
	t.Run("kubevpnConnect", kubevpnConnect)
	t.Run("commonTest", commonTest)
	t.Run("checkConnectStatus", checkConnectStatus)

	// 2) test proxy mode
	t.Run("kubevpnProxy", kubevpnProxy)
	t.Run("commonTest", commonTest)
	t.Run("testUDP", testUDP)
	t.Run("proxyServiceReviewsServiceIP", proxyServiceReviewsServiceIP)
	t.Run("proxyServiceReviewsPodIP", proxyServiceReviewsPodIP)
	t.Run("checkProxyStatus", checkProxyStatus)

	// 3) test proxy mode with service mesh
	t.Run("kubevpnLeave", kubevpnLeave)
	t.Run("kubevpnProxyWithServiceMesh", kubevpnProxyWithServiceMesh)
	t.Run("commonTest", commonTest)
	t.Run("serviceMeshReviewsServiceIP", serviceMeshReviewsServiceIP)
	t.Run("serviceMeshReviewsPodIP", serviceMeshReviewsPodIP)
	t.Run("checkProxyWithServiceMeshStatus", checkProxyWithServiceMeshStatus)

	// 4) test proxy mode with service mesh and gvisor
	t.Run("kubevpnLeave", kubevpnLeave)
	t.Run("kubevpnUninstall", kubevpnUninstall)
	t.Run("kubevpnProxyWithServiceMeshAndGvisorMode", kubevpnProxyWithServiceMeshAndGvisorMode)
	t.Run("commonTest", commonTest)
	t.Run("serviceMeshReviewsServiceIP", serviceMeshReviewsServiceIP)
	t.Run("checkProxyWithServiceMeshAndGvisorStatus", checkProxyWithServiceMeshAndGvisorStatus)
	t.Run("kubevpnLeaveService", kubevpnLeaveService)
	t.Run("kubevpnQuit", kubevpnQuit)

	// 5) install centrally in ns test -- connect mode
	t.Run("centerKubevpnUninstall", kubevpnUninstall)
	t.Run("centerKubevpnInstallInNsKubevpn", kubevpnConnectToNsKubevpn)
	t.Run("centerKubevpnConnect", kubevpnConnect)
	t.Run("checkServiceShouldNotInNsDefault", checkServiceShouldNotInNsDefault)
	t.Run("centerCheckConnectStatus", centerCheckConnectStatus)
	t.Run("centerCommonTest", commonTest)

	// 6) install centrally in ns test -- proxy mode
	t.Run("centerKubevpnProxy", kubevpnProxy)
	t.Run("checkServiceShouldNotInNsDefault", checkServiceShouldNotInNsDefault)
	t.Run("centerCommonTest", commonTest)
	t.Run("centerTestUDP", testUDP)
	t.Run("centerProxyServiceReviewsServiceIP", proxyServiceReviewsServiceIP)
	t.Run("centerProxyServiceReviewsPodIP", proxyServiceReviewsPodIP)
	t.Run("centerCheckProxyStatus", centerCheckProxyStatus)

	// 7) install centrally in ns test -- proxy mode with service mesh
	t.Run("kubevpnLeave", kubevpnLeave)
	t.Run("kubevpnProxyWithServiceMesh", kubevpnProxyWithServiceMesh)
	t.Run("checkServiceShouldNotInNsDefault", checkServiceShouldNotInNsDefault)
	t.Run("commonTest", commonTest)
	t.Run("serviceMeshReviewsServiceIP", serviceMeshReviewsServiceIP)
	t.Run("serviceMeshReviewsPodIP", serviceMeshReviewsPodIP)
	t.Run("centerCheckProxyWithServiceMeshStatus", centerCheckProxyWithServiceMeshStatus)

	// 8) install centrally in ns test -- proxy mode with service mesh and gvisor
	t.Run("kubevpnQuit", kubevpnQuit)
	t.Run("kubevpnProxyWithServiceMeshAndK8sServicePortMap", kubevpnProxyWithServiceMeshAndK8sServicePortMap)
	t.Run("checkServiceShouldNotInNsDefault", checkServiceShouldNotInNsDefault)
	t.Run("commonTest", commonTest)
	t.Run("serviceMeshReviewsServiceIPPortMap", serviceMeshReviewsServiceIPPortMap)
	t.Run("kubevpnLeave", kubevpnLeave)
	t.Run("centerCheckProxyWithServiceMeshAndGvisorStatus", centerCheckProxyWithServiceMeshAndGvisorStatus)
	t.Run("kubevpnLeaveService", kubevpnLeaveService)
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

	client := &http.Client{Timeout: time.Second * 1}
	err = retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 1, Jitter: 0, Steps: 120},
		func(err error) bool { return err != nil },
		func() error {
			var resp *http.Response
			resp, err = client.Do(req)
			if err != nil {
				t.Logf("%s failed to do health check endpoint: %s: %v", time.Now().Format(time.DateTime), endpoint, err)
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

func serviceMeshReviewsServiceIPPortMap(t *testing.T) {
	app := "reviews"
	ip, err := getServiceIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	healthChecker(t, endpoint, nil, remote)
	healthChecker(t, endpoint, map[string]string{"env": "test"}, local8080)
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
	port, err := util.GetAvailableUDPPortOrDie()
	if err != nil {
		t.Fatal(err)
	}
	go udpServer(t, port)

	var ip string
	err = retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.2, Steps: 5},
		func(err error) bool {
			return err != nil
		},
		func() error {
			ip, err = getPodIP(app)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("Dail udp to IP: %s", ip)
			return udpClient(t, ip, port)
		})
	if err != nil {
		t.Fatalf("Failed to access pod IP: %s, port: %v", ip, port)
	}
}

func udpClient(t *testing.T, ip string, port int) error {
	udpConn, err := net.DialUDP("udp4", nil, &net.UDPAddr{
		IP:   net.ParseIP(ip),
		Port: port,
	})
	if err != nil {
		return err
	}
	defer udpConn.Close()

	err = udpConn.SetDeadline(time.Now().Add(time.Second * 30))
	if err != nil {
		return err
	}

	sendData := []byte("hello server!")
	_, err = udpConn.Write(sendData)
	if err != nil {
		t.Logf("Failed to send udp packet: %v", err)
		return err
	}

	data := make([]byte, 4096)
	read, remoteAddr, err := udpConn.ReadFromUDP(data)
	if err != nil {
		t.Logf("Failed to read udp packet: %v", err)
		return err
	}
	t.Logf("read data from %v: %v", remoteAddr, string(data[:read]))
	return nil
}

func udpServer(t *testing.T, port int) {
	// 创建监听
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: port,
	})
	if err != nil {
		t.Fatal(err)
		return
	}
	defer udpConn.Close()

	data := make([]byte, 4096)
	for {
		read, remoteAddr, err := udpConn.ReadFromUDP(data[:])
		if err != nil {
			t.Logf("failed to read udp data from %v: %v", remoteAddr, err)
			continue
		}
		t.Logf("read data from %v: %v", remoteAddr, string(data[:read]))

		sendData := []byte("hello client!")
		_, err = udpConn.WriteToUDP(sendData, remoteAddr)
		if err != nil {
			t.Logf("failed to send udp data to %v: %v", remoteAddr, err)
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

func kubevpnConnectToNsKubevpn(t *testing.T) {
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: v1.ObjectMeta{
			Name: "kubevpn",
		},
	}, v1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cmdConnect := exec.Command("kubevpn", "connect", "--namespace", "kubevpn", "--debug")
	cmdQuit := exec.Command("kubevpn", "quit")
	for _, cmd := range []*exec.Cmd{cmdConnect, cmdQuit} {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		if err != nil {
			t.Fatal(err)
		}
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
	cmd := exec.Command("kubevpn", "proxy", "svc/reviews", "--headers", "env=test", "--debug")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func kubevpnProxyWithServiceMeshAndK8sServicePortMap(t *testing.T) {
	cmd := exec.Command("kubevpn", "proxy", "svc/reviews", "--headers", "env=test", "--debug", "--portmap", "9080:8080")
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

func kubevpnLeaveService(t *testing.T) {
	cmd := exec.Command("kubevpn", "leave", "services/reviews")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func checkConnectStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: namespace,
		Status:    "connected",
		ProxyList: nil,
	}}}

	var statuses status
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(statuses, expect) {
		marshal, _ := json.Marshal(expect)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
	}
}
func centerCheckConnectStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: "default",
		Status:    "connected",
		ProxyList: nil,
	}}}

	var statuses status
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(statuses, expect) {
		marshal, _ := json.Marshal(expect)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
	}
}

type status struct {
	List []*connection
}

type connection struct {
	Namespace string
	Status    string
	ProxyList []*proxy
}
type proxy struct {
	Namespace string
	Workload  string
	RuleList  []*rule
}
type rule struct {
	Headers       map[string]string
	CurrentDevice bool
	PortMap       map[int32]int32
}

func checkProxyStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: namespace,
		Status:    "connected",
		ProxyList: []*proxy{{
			Namespace: namespace,
			Workload:  "deployments.apps/reviews",
			RuleList: []*rule{{
				Headers:       nil,
				CurrentDevice: true,
				PortMap:       map[int32]int32{9080: 9080},
			}},
		}},
	}}}

	var statuses status
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(statuses, expect) {
		marshal, _ := json.Marshal(expect)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
	}
}

func centerCheckProxyStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: "default",
		Status:    "connected",
		ProxyList: []*proxy{{
			Namespace: "default",
			Workload:  "deployments.apps/reviews",
			RuleList: []*rule{{
				Headers:       nil,
				CurrentDevice: true,
				PortMap:       map[int32]int32{9080: 9080},
			}},
		}},
	}}}

	var statuses status
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(statuses, expect) {
		marshal, _ := json.Marshal(expect)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
	}
}

func checkProxyWithServiceMeshStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: namespace,
		Status:    "connected",
		ProxyList: []*proxy{{
			Namespace: namespace,
			Workload:  "deployments.apps/reviews",
			RuleList: []*rule{{
				Headers:       map[string]string{"env": "test"},
				CurrentDevice: true,
				PortMap:       map[int32]int32{9080: 9080},
			}},
		}},
	}}}

	var statuses status
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(statuses, expect) {
		marshal, _ := json.Marshal(expect)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
	}
}

func centerCheckProxyWithServiceMeshStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: "default",
		Status:    "connected",
		ProxyList: []*proxy{{
			Namespace: "default",
			Workload:  "deployments.apps/reviews",
			RuleList: []*rule{{
				Headers:       map[string]string{"env": "test"},
				CurrentDevice: true,
				PortMap:       map[int32]int32{9080: 9080},
			}},
		}},
	}}}

	var statuses status
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(statuses, expect) {
		marshal, _ := json.Marshal(expect)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
	}
}

func checkProxyWithServiceMeshAndGvisorStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: namespace,
		Status:    "connected",
		ProxyList: []*proxy{{
			Namespace: namespace,
			Workload:  "services/reviews",
			RuleList: []*rule{{
				Headers:       map[string]string{"env": "test"},
				CurrentDevice: true,
				PortMap:       map[int32]int32{9080: 9080},
			}},
		}},
	}}}

	var statuses status
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}
	opt := cmp.FilterPath(func(p cmp.Path) bool {
		vx := p.Last().String()
		if vx == `["Headers"]` {
			return true
		}
		return false
	}, cmp.Ignore())
	if !cmp.Equal(statuses, expect, opt) {
		marshal, _ := json.Marshal(expect)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
	}
}

func centerCheckProxyWithServiceMeshAndGvisorStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: "default",
		Status:    "connected",
		ProxyList: []*proxy{{
			Namespace: "default",
			Workload:  "services/reviews",
			RuleList: []*rule{{
				Headers:       map[string]string{"env": "test"},
				CurrentDevice: true,
				PortMap:       map[int32]int32{9080: 8080},
			}},
		}},
	}}}

	var statuses status
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}
	opt := cmp.FilterPath(func(p cmp.Path) bool {
		vx := p.Last().String()
		if vx == `["Headers"]` {
			return true
		}
		return false
	}, cmp.Ignore())
	if !cmp.Equal(statuses, expect, opt) {
		marshal, _ := json.Marshal(expect)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
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

func checkServiceShouldNotInNsDefault(t *testing.T) {
	_, err := clientset.CoreV1().Services(namespace).Get(context.Background(), pkgconfig.ConfigMapPodTrafficManager, v1.GetOptions{})
	if !k8serrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func kubectl(t *testing.T) {
	cmdGetPod := exec.Command("kubectl", "get", "pods", "-o", "wide")
	cmdDescribePod := exec.Command("kubectl", "describe", "pods")
	cmdGetSvc := exec.Command("kubectl", "get", "services", "-o", "wide")
	cmdDescribeSvc := exec.Command("kubectl", "describe", "services")
	for _, cmd := range []*exec.Cmd{cmdGetPod, cmdDescribePod, cmdGetSvc, cmdDescribeSvc} {
		t.Logf("exec: %v", cmd.Args)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func Init(t *testing.T) {
	var err error

	configFlags := genericclioptions.NewConfigFlags(true)
	f := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))

	if restconfig, err = f.ToRESTConfig(); err != nil {
		t.Fatal(err)
	}
	if clientset, err = kubernetes.NewForConfig(restconfig); err != nil {
		t.Fatal(err)
	}
	if namespace, _, err = f.ToRawKubeConfigLoader().Namespace(); err != nil {
		t.Fatal(err)
	}

	go startupHttpServer(t, "localhost:9080", local)
	go startupHttpServer(t, "localhost:8080", local8080)
}

func startupHttpServer(t *testing.T, addr, str string) {
	var health = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(str))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", health)
	mux.HandleFunc("/health", health)
	t.Logf("Start listening http addr %s ...", addr)
	err := http.ListenAndServe(addr, mux)
	if err != nil {
		t.Fatal(err)
	}
}
