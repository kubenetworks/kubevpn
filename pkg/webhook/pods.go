package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/mattbaird/jsonpatch"
	"k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/cmd/util/podcmd"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
)

// only allow pods to pull images from specific registry.
func admitPods(ar v1.AdmissionReview) *v1.AdmissionResponse {
	klog.V(2).Info("admitting pods")
	podResource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	if ar.Request.Resource != podResource {
		err := fmt.Errorf("expect resource to be %s", podResource)
		klog.Error(err)
		return toV1AdmissionResponse(err)
	}

	raw := ar.Request.Object.Raw
	pod := corev1.Pod{}

	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
		klog.Error(err)
		return toV1AdmissionResponse(err)
	}

	switch ar.Request.Operation {
	case v1.Create:
		from, _ := json.Marshal(pod)
		fmt.Println(ar.String())
		var found bool
		for i := 0; i < len(pod.Spec.Containers); i++ {
			if pod.Spec.Containers[i].Name == config.ContainerSidecarVPN {
				for j := 0; j < len(pod.Spec.Containers[i].Env); j++ {
					pair := pod.Spec.Containers[i].Env[j]
					if pair.Name == "InboundPodTunIP" {
						found = true
						conf, err := rest.InClusterConfig()
						if err != nil {
							klog.Error(err)
							return toV1AdmissionResponse(err)
						}
						clientset, err := kubernetes.NewForConfig(conf)
						if err != nil {
							klog.Error(err)
							return toV1AdmissionResponse(err)
						}
						cmi := clientset.CoreV1().ConfigMaps(ar.Request.Namespace)
						if cmi == nil {
							err = fmt.Errorf("why cmi is nil")
							klog.Error(err)
							return toV1AdmissionResponse(err)
						}
						_, err = cmi.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
						if err != nil {
							klog.Error(err)
							return toV1AdmissionResponse(err)
						}
						dhcp := handler.NewDHCPManager(cmi, ar.Request.Namespace, &net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask})
						random, err := dhcp.RentIPRandom()
						if err != nil {
							klog.Error(err)
							return toV1AdmissionResponse(err)
						}
						pod.Spec.Containers[i].Env[j].Value = random.String()
					}
				}
			}
		}
		fmt.Println(found)
		if found {
			to, _ := json.Marshal(pod)
			patch, _ := jsonpatch.CreatePatch(from, to)
			marshal, _ := json.Marshal(patch)
			return applyPodPatch(ar, func(pod *corev1.Pod) bool {
				name, _ := podcmd.FindContainerByName(pod, config.ContainerSidecarVPN)
				return name != nil
			}, string(marshal))
		}
		return &v1.AdmissionResponse{
			Allowed: true,
		}
	case v1.Delete:
		name, _ := podcmd.FindContainerByName(&pod, config.ContainerSidecarVPN)
		if name != nil {
			for _, envVar := range name.Env {
				if envVar.Name == "InboundPodTunIP" {
					ip, cidr, err := net.ParseCIDR(envVar.Value)
					if err == nil {
						conf, err := rest.InClusterConfig()
						if err != nil {
							klog.Error(err)
							return toV1AdmissionResponse(err)
						}
						clientset, err := kubernetes.NewForConfig(conf)
						if err != nil {
							klog.Error(err)
							return toV1AdmissionResponse(err)
						}
						cmi := clientset.CoreV1().ConfigMaps(ar.Request.Namespace)
						if cmi == nil {
							err = fmt.Errorf("why cmi is nil")
							klog.Error(err)
							return toV1AdmissionResponse(err)
						}
						ipnet := &net.IPNet{
							IP:   ip,
							Mask: cidr.Mask,
						}
						err = handler.NewDHCPManager(cmi, ar.Request.Namespace, &net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask}).ReleaseIpToDHCP(ipnet)
						if err != nil {
							klog.V(1).Infof("release ip to dhcp err: %v", err)
						}
					}
				}
			}
		}

		return &v1.AdmissionResponse{
			Allowed: true,
		}
	default:
		return toV1AdmissionResponse(fmt.Errorf("expect operation is %s or %s, not %s", v1.Create, v1.Delete, ar.Request.Operation))
	}
}

func applyPodPatch(ar v1.AdmissionReview, shouldPatchPod func(*corev1.Pod) bool, patch string) *v1.AdmissionResponse {
	klog.V(2).Info("mutating pods")
	podResource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	if ar.Request.Resource != podResource {
		klog.Errorf("expect resource to be %s", podResource)
		return nil
	}

	raw := ar.Request.Object.Raw
	pod := corev1.Pod{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
		klog.Error(err)
		return toV1AdmissionResponse(err)
	}
	reviewResponse := v1.AdmissionResponse{}
	reviewResponse.Allowed = true
	if shouldPatchPod(&pod) {
		reviewResponse.Patch = []byte(patch)
		pt := v1.PatchTypeJSONPatch
		reviewResponse.PatchType = &pt
	}
	return &reviewResponse
}

// denySpecificAttachment denies `kubectl attach to-be-attached-pod -i -c=container1"
// or equivalent client requests.
func denySpecificAttachment(ar v1.AdmissionReview) *v1.AdmissionResponse {
	klog.V(2).Info("handling attaching pods")
	if ar.Request.Name != "to-be-attached-pod" {
		return &v1.AdmissionResponse{Allowed: true}
	}
	podResource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	if e, a := podResource, ar.Request.Resource; e != a {
		err := fmt.Errorf("expect resource to be %s, got %s", e, a)
		klog.Error(err)
		return toV1AdmissionResponse(err)
	}
	if e, a := "attach", ar.Request.SubResource; e != a {
		err := fmt.Errorf("expect subresource to be %s, got %s", e, a)
		klog.Error(err)
		return toV1AdmissionResponse(err)
	}

	raw := ar.Request.Object.Raw
	podAttachOptions := corev1.PodAttachOptions{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &podAttachOptions); err != nil {
		klog.Error(err)
		return toV1AdmissionResponse(err)
	}
	klog.V(2).Info(fmt.Sprintf("podAttachOptions=%#v\n", podAttachOptions))
	if !podAttachOptions.Stdin || podAttachOptions.Container != "container1" {
		return &v1.AdmissionResponse{Allowed: true}
	}
	return &v1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Message: "attaching to pod 'to-be-attached-pod' is not allowed",
		},
	}
}
