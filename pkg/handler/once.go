package handler

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func Once(ctx context.Context, factory cmdutil.Factory) error {
	clientset, err := factory.KubernetesClientSet()
	if err != nil {
		return err
	}
	namespace, _, err := factory.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	err = labelNs(ctx, namespace, clientset)
	if err != nil {
		return err
	}
	err = genTLS(ctx, namespace, clientset)
	if err != nil {
		return err
	}
	err = restartDeployment(ctx, namespace, clientset)
	if err != nil {
		return err
	}
	err = getCIDR(ctx, factory)
	if err != nil {
		return err
	}
	return nil
}

func labelNs(ctx context.Context, namespace string, clientset *kubernetes.Clientset) error {
	plog.G(ctx).Infof("Labeling Namespace %s", namespace)
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to get namespace: %v", err)
		return err
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	if ns.Labels["ns"] == namespace {
		plog.G(ctx).Infof("Namespace %s already labeled", namespace)
		return nil
	}
	ns.Labels["ns"] = namespace
	_, err = clientset.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
	if err != nil {
		plog.G(ctx).Infof("Failed to labele namespace: %v", err)
		return err
	}
	return nil
}

func genTLS(ctx context.Context, namespace string, clientset *kubernetes.Clientset) error {
	plog.G(ctx).Infof("Generating TLS for Namespace %s", namespace)
	crt, key, host, err := util.GenTLSCert(ctx, namespace)
	if err != nil {
		return err
	}
	// reason why not use v1.SecretTypeTls is because it needs key called tls.crt and tls.key, but tls.key can not as env variable
	// ➜  ~ export tls.key=a
	//export: not valid in this context: tls.key
	secret := genSecret(namespace, crt, key, host)
	oldSecret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, secret.Name, metav1.GetOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to get secret: %v", err)
		return err
	}
	secret.ResourceVersion = oldSecret.ResourceVersion
	_, err = clientset.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to update secret: %v", err)
		return err
	}

	mutatingWebhookConfiguration := genMutatingWebhookConfiguration(namespace, crt)
	oldConfig, err := clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, mutatingWebhookConfiguration.Name, metav1.GetOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to get mutatingWebhookConfiguration: %v", err)
		return err
	}
	mutatingWebhookConfiguration.ResourceVersion = oldConfig.ResourceVersion
	_, err = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Update(ctx, mutatingWebhookConfiguration, metav1.UpdateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to update mutatingWebhookConfiguration: %v", err)
		return err
	}
	return nil
}

func restartDeployment(ctx context.Context, namespace string, clientset *kubernetes.Clientset) error {
	deployName := config.ConfigMapPodTrafficManager
	plog.G(ctx).Infof("Restarting Deployment %s", deployName)
	scale, err := clientset.AppsV1().Deployments(namespace).GetScale(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to get scale: %v", err)
		return err
	}
	scale.Spec.Replicas = 0
	scale, err = clientset.AppsV1().Deployments(namespace).UpdateScale(ctx, deployName, scale, metav1.UpdateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to update scale: %v", err)
		return err
	}
	scale.Spec.Replicas = 1
	_, err = clientset.AppsV1().Deployments(namespace).UpdateScale(ctx, deployName, scale, metav1.UpdateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to update scale: %v", err)
		return err
	}
	return nil
}

func getCIDR(ctx context.Context, factory cmdutil.Factory) error {
	plog.G(ctx).Infof("Getting CIDR")
	c := &ConnectOptions{
		Image: config.Image,
	}
	err := c.InitClient(factory)
	if err != nil {
		return err
	}
	// run inside pod
	err = c.getCIDR(ctx, false)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get CIDR: %v", err)
		return err
	}
	s := sets.New[string]()
	for _, cidr := range c.cidrs {
		s.Insert(cidr.String())
	}
	plog.G(ctx).Infof("Get CIDR: %v", strings.Join(s.UnsortedList(), " "))
	return nil
}
