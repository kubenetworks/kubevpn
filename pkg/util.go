package main

import (
	"context"
	"fmt"
	"github.com/moby/term"
	log "github.com/sirupsen/logrus"
	v12 "k8s.io/api/autoscaling/v1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	clientgowatch "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/util"
	term2 "k8s.io/kubectl/pkg/util/term"
	"net"
	"net/http"
	"os"
	"time"
)

func WaitResource(client *kubernetes.Clientset, getter cache.Getter, namespace, apiVersion, kind string, list metav1.ListOptions, checker func(interface{}) bool) error {
	groupResources, _ := restmapper.GetAPIGroupResources(client)
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)
	groupVersionKind := schema.FromAPIVersionAndKind(apiVersion, kind)
	mapping, err := mapper.RESTMapping(groupVersionKind.GroupKind(), groupVersionKind.Version)
	if err != nil {
		log.Error(err)
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	watchlist := cache.NewFilteredListWatchFromClient(
		getter,
		mapping.Resource.Resource,
		namespace,
		func(options *metav1.ListOptions) {
			options.LabelSelector = list.LabelSelector
			options.FieldSelector = list.FieldSelector
			options.Watch = list.Watch
		},
	)

	preConditionFunc := func(store cache.Store) (bool, error) {
		if len(store.List()) == 0 {
			return false, nil
		}
		for _, p := range store.List() {
			if !checker(p) {
				return false, nil
			}
		}
		return true, nil
	}

	conditionFunc := func(e watch.Event) (bool, error) { return checker(e.Object), nil }

	object, err := scheme.Scheme.New(mapping.GroupVersionKind)
	if err != nil {
		return err
	}

	event, err := clientgowatch.UntilWithSync(ctx, watchlist, object, preConditionFunc, conditionFunc)
	if err != nil {
		log.Infof("wait to ready failed, error: %v, event: %v", err, event)
		return err
	}
	return nil
}

func GetAvailablePortOrDie() int {
	address, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:0", "0.0.0.0"))
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func WaitPod(clientset *kubernetes.Clientset, namespace string, list metav1.ListOptions, checker func(*v1.Pod) bool) error {
	return WaitResource(
		clientset,
		clientset.CoreV1().RESTClient(),
		namespace,
		"v1",
		"Pod",
		list,
		func(i interface{}) bool { return checker(i.(*v1.Pod)) },
	)
}

func PortForwardPod(config *rest.Config, clientset *kubernetes.Clientset, podName, namespace, portPair string, readyChan, stopChan chan struct{}) error {
	url := clientset.CoreV1().
		RESTClient().
		Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").
		URL()
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		log.Error(err)
		return err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)
	p := []string{portPair}
	forwarder, err := portforward.New(dialer, p, stopChan, readyChan, os.Stdout, os.Stderr)
	if err != nil {
		log.Error(err)
		return err
	}

	if err = forwarder.ForwardPorts(); err != nil {
		log.Error(err)
		return err
	}
	return nil
}

func ScaleDeploymentReplicasTo(options *kubernetes.Clientset, name, namespace string, replicas int32) {
	err := retry.OnError(
		retry.DefaultRetry,
		func(err error) bool { return err != nil },
		func() error {
			_, err := options.AppsV1().Deployments(namespace).
				UpdateScale(context.TODO(), name, &v12.Scale{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
					Spec:       v12.ScaleSpec{Replicas: replicas},
				}, metav1.UpdateOptions{})
			return err
		})
	if err != nil {
		log.Errorf("update deployment: %s's replicas to %d failed, error: %v", name, replicas, err)
	}
}

type shellOptions interface {
	GetNamespace() string
	GetDeployment() string
	GetLocalDir() string
	GetRemoteDir() string
	GetKubeconfig() string
}

func Shell(client *kubernetes.Clientset, options shellOptions) error {
	deployment, err2 := client.AppsV1().Deployments(options.GetNamespace()).
		Get(context.TODO(), options.GetDeployment(), metav1.GetOptions{})
	if err2 != nil {
		log.Error(err2)
	}
	labelMap, _ := metav1.LabelSelectorAsMap(deployment.Spec.Selector)
	pods, err := client.CoreV1().Pods(options.GetNamespace()).
		List(context.TODO(), metav1.ListOptions{LabelSelector: labels.SelectorFromSet(labelMap).String()})
	if err != nil {
		log.Errorf("get kubedev pod error: %v", err)
	}
	if len(pods.Items) <= 0 {
		log.Warnf("this should not happened, pods items length: %d", len(pods.Items))
	}
	index := -1
	for i, pod := range pods.Items {
		if pod.Status.Phase == v1.PodRunning {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("cannot exec into a container in a completed pod; current phase is %s", pods.Items[0].Status.Phase)
	}
	stdin, stdout, stderr := term.StdStreams()
	tty := term2.TTY{
		Out: stdout,
		In:  stdin,
		Raw: true,
	}
	if !tty.IsTerminalIn() {
		log.Error("Unable to use a TTY - input is not a terminal or the right kind of file")
	}
	var terminalSizeQueue remotecommand.TerminalSizeQueue
	if tty.Raw {
		terminalSizeQueue = tty.MonitorSize(tty.GetSize())
	}
	f := func() error {
		configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
		kubeconfig := options.GetKubeconfig()
		configFlags.KubeConfig = &kubeconfig
		namespace := options.GetNamespace()
		configFlags.Namespace = &namespace
		f := util.NewFactory(util.NewMatchVersionFlags(configFlags))
		config, _ := f.ToRESTConfig()
		restClient, err := rest.RESTClientFor(config)
		if err != nil {
			return err
		}
		req := restClient.Post().
			Resource("pods").
			Name(pods.Items[index].Name).
			Namespace(options.GetNamespace()).
			SubResource("exec").
			VersionedParams(
				&v1.PodExecOptions{
					Container: pods.Items[index].Spec.Containers[0].Name,
					Command:   []string{"sh", "-c", "(bash||sh)"},
					Stdin:     true,
					Stdout:    true,
					Stderr:    true,
					TTY:       true,
				},
				scheme.ParameterCodec,
			)
		executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
		if err != nil {
			return err
		}
		return executor.Stream(remotecommand.StreamOptions{
			Stdin:             tty.In,
			Stdout:            tty.Out,
			Stderr:            stderr,
			Tty:               true,
			TerminalSizeQueue: terminalSizeQueue,
		})
	}

	if err = tty.Safe(f); err != nil {
		return err
	}
	return nil
}
