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
	"runtime"
	"strings"
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

type sshUt struct {
	namespace  string
	clientset  *kubernetes.Clientset
	restconfig *rest.Config
}

func TestSSHFunctions(t *testing.T) {
	u := &sshUt{}
	// 1) test connect
	t.Run("init", u.init)
	t.Run("kubevpnConnect", u.kubevpnConnect)
	t.Run("commonTest", u.commonTest)
	t.Run("checkConnectStatus", u.checkConnectStatus)

	// 2) test proxy mode
	t.Run("kubevpnProxy", u.kubevpnProxy)
	t.Run("commonTest", u.commonTest)
	t.Run("testUDP", u.testUDP)
	t.Run("proxyServiceReviewsServiceIP", u.proxyServiceReviewsServiceIP)
	t.Run("proxyServiceReviewsPodIP", u.proxyServiceReviewsPodIP)
	t.Run("checkProxyStatus", u.checkProxyStatus)

	// 3) test proxy mode with service mesh
	t.Run("kubevpnLeave", u.kubevpnLeave)
	t.Run("kubevpnProxyWithServiceMesh", u.kubevpnProxyWithServiceMesh)
	t.Run("commonTest", u.commonTest)
	t.Run("serviceMeshReviewsServiceIP", u.serviceMeshReviewsServiceIP)
	t.Run("serviceMeshReviewsPodIP", u.serviceMeshReviewsPodIP)
	t.Run("checkProxyWithServiceMeshStatus", u.checkProxyWithServiceMeshStatus)

	// 4) test proxy mode with service mesh and gvisor
	t.Run("kubevpnLeave", u.kubevpnLeave)
	t.Run("kubevpnUninstall", u.kubevpnUninstall(""))
	t.Run("kubevpnProxyWithServiceMeshAndFargateMode", u.kubevpnProxyWithServiceMeshAndFargateMode)
	t.Run("commonTest", u.commonTest)
	t.Run("serviceMeshReviewsServiceIP", u.serviceMeshReviewsServiceIP)
	t.Run("checkProxyWithServiceMeshAndGvisorStatus", u.checkProxyWithServiceMeshAndGvisorStatus)
	t.Run("kubevpnLeaveService", u.kubevpnLeaveService)
	t.Run("kubevpnQuit", u.kubevpnQuit)

	// 5) test mode sync
	t.Run("deleteDeployForSaveResource", u.deleteDeployForSaveResource)
	t.Run("kubevpnSyncWithFullProxy", u.kubevpnSyncWithFullProxy)
	t.Run("kubevpnSyncWithFullProxyStatus", u.checkSyncWithFullProxyStatus)
	t.Run("commonTest", u.commonTest)
	t.Run("kubevpnUnSync", u.kubevpnUnSync)
	t.Run("kubevpnSyncWithServiceMesh", u.kubevpnSyncWithServiceMesh)
	t.Run("kubevpnSyncWithServiceMeshStatus", u.checkSyncWithServiceMeshStatus)
	t.Run("commonTest", u.commonTest)
	t.Run("kubevpnUnSync", u.kubevpnUnSync)

	// 6) test mode run
	t.Run("resetDeployAuthors", u.resetDeployAuthors)
	t.Run("kubevpnRunWithFullProxy", u.kubevpnRunWithFullProxy)
	t.Run("kubevpnRunWithServiceMesh", u.kubevpnRunWithServiceMesh)
	t.Run("kubevpnQuit", u.kubevpnQuit)

	// 7) install centrally in ns kubevpn -- connect mode
	t.Run("centerKubevpnUninstall", u.kubevpnUninstall(""))
	t.Run("centerKubevpnInstallInNsKubevpn", u.kubevpnConnectToNsKubevpn)
	t.Run("centerKubevpnConnect", u.kubevpnConnect)
	t.Run("checkServiceShouldNotInNsDefault", u.checkServiceShouldNotInNsDefault)
	t.Run("centerCheckConnectStatus", u.centerCheckConnectStatus)
	t.Run("centerCommonTest", u.commonTest)

	// 8) install centrally in ns kubevpn -- proxy mode
	t.Run("centerKubevpnProxy", u.kubevpnProxy)
	t.Run("checkServiceShouldNotInNsDefault", u.checkServiceShouldNotInNsDefault)
	t.Run("centerCommonTest", u.commonTest)
	t.Run("centerTestUDP", u.testUDP)
	t.Run("centerProxyServiceReviewsServiceIP", u.proxyServiceReviewsServiceIP)
	t.Run("centerProxyServiceReviewsPodIP", u.proxyServiceReviewsPodIP)
	t.Run("centerCheckProxyStatus", u.centerCheckProxyStatus)

	// 9) install centrally in ns kubevpn -- proxy mode with service mesh
	t.Run("kubevpnLeave", u.kubevpnLeave)
	t.Run("kubevpnProxyWithServiceMesh", u.kubevpnProxyWithServiceMesh)
	t.Run("checkServiceShouldNotInNsDefault", u.checkServiceShouldNotInNsDefault)
	t.Run("commonTest", u.commonTest)
	t.Run("serviceMeshReviewsServiceIP", u.serviceMeshReviewsServiceIP)
	t.Run("serviceMeshReviewsPodIP", u.serviceMeshReviewsPodIP)
	t.Run("centerCheckProxyWithServiceMeshStatus", u.centerCheckProxyWithServiceMeshStatus)

	// 10) install centrally in ns kubevpn -- proxy mode with service mesh and gvisor
	t.Run("kubevpnQuit", u.kubevpnQuit)
	t.Run("kubevpnProxyWithServiceMeshAndK8sServicePortMap", u.kubevpnProxyWithServiceMeshAndK8sServicePortMap)
	t.Run("checkServiceShouldNotInNsDefault", u.checkServiceShouldNotInNsDefault)
	t.Run("commonTest", u.commonTest)
	t.Run("serviceMeshReviewsServiceIPPortMap", u.serviceMeshReviewsServiceIPPortMap)
	t.Run("kubevpnLeave", u.kubevpnLeave)
	t.Run("centerCheckProxyWithServiceMeshAndGvisorStatus", u.centerCheckProxyWithServiceMeshAndGvisorStatus)
	t.Run("kubevpnLeaveService", u.kubevpnLeaveService)
	t.Run("kubevpnQuit", u.kubevpnQuit)

	// 11) test mode sync
	t.Run("kubevpnSyncWithFullProxy", u.kubevpnSyncWithFullProxy)
	t.Run("checkServiceShouldNotInNsDefault", u.checkServiceShouldNotInNsDefault)
	t.Run("kubevpnSyncWithFullProxyStatus", u.checkSyncWithFullProxyStatus)
	t.Run("commonTest", u.commonTest)
	t.Run("kubevpnUnSync", u.kubevpnUnSync)
	t.Run("kubevpnSyncWithServiceMesh", u.kubevpnSyncWithServiceMesh)
	t.Run("checkServiceShouldNotInNsDefault", u.checkServiceShouldNotInNsDefault)
	t.Run("kubevpnSyncWithServiceMeshStatus", u.checkSyncWithServiceMeshStatus)
	t.Run("commonTest", u.commonTest)
	t.Run("kubevpnUnSync", u.kubevpnUnSync)
	t.Run("kubevpnQuit", u.kubevpnQuit)

	// 12) test mode run
	t.Run("resetDeployAuthors", u.resetDeployAuthors)
	t.Run("kubevpnRunWithFullProxy", u.kubevpnRunWithFullProxy)
	t.Run("kubevpnRunWithServiceMesh", u.kubevpnRunWithServiceMesh)
	t.Run("kubevpnQuit", u.kubevpnQuit)
	t.Run("kubevpnUninstall", u.kubevpnUninstall("kubevpn"))
	t.Run("kubevpnQuit", u.kubevpnQuit)
}

func (u *sshUt) commonTest(t *testing.T) {
	// 1) test domain access
	t.Run("kubevpnStatus", u.kubevpnStatus)
	t.Run("pingPodIP", u.pingPodIP)
	t.Run("healthCheckPodDetails", u.healthCheckPodDetails)
	t.Run("healthCheckServiceDetails", u.healthCheckServiceDetails)
	t.Run("shortDomainDetails", u.shortDomainDetails)
	t.Run("fullDomainDetails", u.fullDomainDetails)
}

func (u *sshUt) pingPodIP(t *testing.T) {
	list, err := u.clientset.CoreV1().Pods(u.namespace).List(context.Background(), v1.ListOptions{})
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
			u.kubectl(t)
		}()
	}
	wg.Wait()
}

func (u *sshUt) healthCheckPodDetails(t *testing.T) {
	var app = "details"
	ip, err := u.getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	u.healthChecker(t, endpoint, nil, "")
}

func (u *sshUt) healthChecker(t *testing.T, endpoint string, header map[string]string, keyword string) {
	// 0 = this frame.
	_, file, line, ok := runtime.Caller(1)
	if ok {
		// Trim any directory path from the file.
		slash := strings.LastIndexByte(file, byte('/'))
		if slash >= 0 {
			file = file[slash+1:]
		}
	} else {
		// We don't have a filename.
		file = "???"
		line = 0
	}

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
		u.kubectl(t)
		t.Fatal(fmt.Sprintf("%s:%d", file, line), err)
	}
}

func (u *sshUt) healthCheckServiceDetails(t *testing.T) {
	var app = "details"
	ip, err := u.getServiceIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	u.healthChecker(t, endpoint, nil, "")
}

func (u *sshUt) shortDomainDetails(t *testing.T) {
	var app = "details"
	endpoint := fmt.Sprintf("http://%s:%v/health", app, 9080)
	u.healthChecker(t, endpoint, nil, "")
}

func (u *sshUt) fullDomainDetails(t *testing.T) {
	var app = "details"
	domains := []string{
		fmt.Sprintf("%s.%s.svc.cluster.local", app, u.namespace),
		fmt.Sprintf("%s.%s.svc", app, u.namespace),
		fmt.Sprintf("%s.%s", app, u.namespace),
	}

	for _, domain := range domains {
		endpoint := fmt.Sprintf("http://%s:%v/health", domain, 9080)
		u.healthChecker(t, endpoint, nil, "")
	}
}

func (u *sshUt) serviceMeshReviewsPodIP(t *testing.T) {
	app := "reviews"
	ip, err := u.getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	u.healthChecker(t, endpoint, nil, remote)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, local)
}

func (u *sshUt) serviceMeshReviewsServiceIP(t *testing.T) {
	app := "reviews"
	ip, err := u.getServiceIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	u.healthChecker(t, endpoint, nil, remote)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, local)
}

func (u *sshUt) serviceMeshReviewsServiceIPPortMap(t *testing.T) {
	app := "reviews"
	ip, err := u.getServiceIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	u.healthChecker(t, endpoint, nil, remote)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, local8080)
}

func (u *sshUt) getServiceIP(app string) (string, error) {
	serviceList, err := u.clientset.CoreV1().Services(u.namespace).List(context.Background(), v1.ListOptions{
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

func (u *sshUt) proxyServiceReviewsPodIP(t *testing.T) {
	app := "reviews"
	ip, err := u.getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	u.healthChecker(t, endpoint, nil, local)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, local)
}

func (u *sshUt) getPodIP(app string) (string, error) {
	list, err := u.clientset.CoreV1().Pods(u.namespace).List(context.Background(), v1.ListOptions{
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

func (u *sshUt) proxyServiceReviewsServiceIP(t *testing.T) {
	app := "reviews"
	ip, err := u.getServiceIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	u.healthChecker(t, endpoint, nil, local)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, local)
}

func (u *sshUt) testUDP(t *testing.T) {
	app := "reviews"
	port, err := util.GetAvailableUDPPortOrDie()
	if err != nil {
		t.Fatal(err)
	}
	go u.udpServer(t, port)

	var ip string
	err = retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.2, Steps: 5},
		func(err error) bool {
			return err != nil
		},
		func() error {
			ip, err = u.getPodIP(app)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("Dail udp to IP: %s", ip)
			return u.udpClient(t, ip, port)
		})
	if err != nil {
		t.Fatalf("Failed to access pod IP: %s, port: %v", ip, port)
	}
}

func (u *sshUt) udpClient(t *testing.T, ip string, port int) error {
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

func (u *sshUt) udpServer(t *testing.T, port int) {
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

func (u *sshUt) kubevpnConnect(t *testing.T) {
	cmd := exec.Command("kubevpn", "connect", "--debug", "--ssh-addr", "localhost:2222", "--ssh-username", "naison", "--ssh-password", "naison")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func (u *sshUt) kubevpnConnectToNsKubevpn(t *testing.T) {
	_, err := u.clientset.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
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

func (u *sshUt) kubevpnProxy(t *testing.T) {
	cmd := exec.Command("kubevpn", "proxy", "deployments/reviews", "--debug", "--ssh-addr", "localhost:2222", "--ssh-username", "naison", "--ssh-password", "naison")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func (u *sshUt) kubevpnProxyWithServiceMesh(t *testing.T) {
	cmd := exec.Command("kubevpn", "proxy", "deployments/reviews", "--headers", "env=test", "--debug", "--ssh-addr", "localhost:2222", "--ssh-username", "naison", "--ssh-password", "naison")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func (u *sshUt) kubevpnProxyWithServiceMeshAndFargateMode(t *testing.T) {
	cmd := exec.Command("kubevpn", "proxy", "svc/reviews", "--headers", "env=test", "--debug", "--ssh-addr", "localhost:2222", "--ssh-username", "naison", "--ssh-password", "naison")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func (u *sshUt) kubevpnProxyWithServiceMeshAndK8sServicePortMap(t *testing.T) {
	cmd := exec.Command("kubevpn", "proxy", "svc/reviews", "--headers", "env=test", "--debug", "--portmap", "9080:8080", "--ssh-addr", "localhost:2222", "--ssh-username", "naison", "--ssh-password", "naison")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func (u *sshUt) kubevpnLeave(t *testing.T) {
	cmd := exec.Command("kubevpn", "leave", "deployments/reviews")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func (u *sshUt) kubevpnLeaveService(t *testing.T) {
	cmd := exec.Command("kubevpn", "leave", "services/reviews")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func (u *sshUt) checkConnectStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: u.namespace,
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
func (u *sshUt) centerCheckConnectStatus(t *testing.T) {
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

func (u *sshUt) checkProxyStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: u.namespace,
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: u.namespace,
			Workload:  "deployments.apps/reviews",
			RuleList: []*proxyRule{{
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

func (u *sshUt) centerCheckProxyStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: "default",
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: "default",
			Workload:  "deployments.apps/reviews",
			RuleList: []*proxyRule{{
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

func (u *sshUt) checkProxyWithServiceMeshStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: u.namespace,
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: u.namespace,
			Workload:  "deployments.apps/reviews",
			RuleList: []*proxyRule{{
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

func (u *sshUt) centerCheckProxyWithServiceMeshStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: "default",
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: "default",
			Workload:  "deployments.apps/reviews",
			RuleList: []*proxyRule{{
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

func (u *sshUt) checkProxyWithServiceMeshAndGvisorStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: u.namespace,
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: u.namespace,
			Workload:  "services/reviews",
			RuleList: []*proxyRule{{
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

func (u *sshUt) centerCheckProxyWithServiceMeshAndGvisorStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	expect := status{List: []*connection{{
		Namespace: "default",
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: "default",
			Workload:  "services/reviews",
			RuleList: []*proxyRule{{
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

func (u *sshUt) kubevpnUninstall(ns string) func(t *testing.T) {
	if ns != "" {
		return func(t *testing.T) {
			cmd := exec.Command("kubevpn", "uninstall", "kubevpn", "-n", ns)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			if err != nil {
				t.Fatal(err)
			}
			cmd = exec.Command("kubectl", "delete", "ns", ns, "--wait")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return func(t *testing.T) {
		cmd := exec.Command("kubevpn", "uninstall", "kubevpn")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func (u *sshUt) kubevpnStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func (u *sshUt) kubevpnQuit(t *testing.T) {
	cmd := exec.Command("kubevpn", "quit")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func (u *sshUt) checkServiceShouldNotInNsDefault(t *testing.T) {
	_, err := u.clientset.CoreV1().Services(u.namespace).Get(context.Background(), pkgconfig.ConfigMapPodTrafficManager, v1.GetOptions{})
	if !k8serrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func (u *sshUt) kubectl(t *testing.T) {
	cmdGetPod := exec.Command("kubectl", "get", "pods", "-o", "wide", "-A")
	cmdGetSvc := exec.Command("kubectl", "get", "services", "-o", "wide", "-A")
	cmdDescribePod := exec.Command("kubectl", "describe", "pods", "-A")
	cmdDescribeSvc := exec.Command("kubectl", "describe", "services", "-A")
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

func (u *sshUt) init(t *testing.T) {
	var err error

	configFlags := genericclioptions.NewConfigFlags(true)
	f := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))

	if u.restconfig, err = f.ToRESTConfig(); err != nil {
		t.Fatal(err)
	}
	if u.clientset, err = kubernetes.NewForConfig(u.restconfig); err != nil {
		t.Fatal(err)
	}
	if u.namespace, _, err = f.ToRawKubeConfigLoader().Namespace(); err != nil {
		t.Fatal(err)
	}

	go u.startupHttpServer(t, "localhost:9080", local)
	go u.startupHttpServer(t, "localhost:8080", local8080)
}

func (u *sshUt) startupHttpServer(t *testing.T, addr, str string) {
	var health = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(str))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", health)
	mux.HandleFunc("/health", health)
	t.Logf("Start listening http addr %s ...", addr)
	_ = http.ListenAndServe(addr, mux)
}
