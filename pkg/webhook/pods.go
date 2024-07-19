package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/mattbaird/jsonpatch"
	log "github.com/sirupsen/logrus"
	"k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubectl/pkg/cmd/util/podcmd"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// create pod will rent ip and delete pod will release ip
func (h *admissionReviewHandler) admitPods(ar v1.AdmissionReview) *v1.AdmissionResponse {
	r, _ := json.Marshal(ar)
	log.Infof("admitting pods called, req: %v", string(r))
	podResource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	if ar.Request.Resource != podResource {
		err := fmt.Errorf("expect resource to be %s but real %s", podResource, ar.Request.Resource)
		log.Error(err)
		return toV1AdmissionResponse(err)
	}

	switch ar.Request.Operation {
	case v1.Create:
		return h.handleCreate(ar)

	case v1.Delete:
		return h.handleDelete(ar)

	default:
		err := fmt.Errorf("expect operation is %s or %s, not %s", v1.Create, v1.Delete, ar.Request.Operation)
		log.Error(err)
		return toV1AdmissionResponse(err)
	}
}

// handle create pod event
func (h *admissionReviewHandler) handleCreate(ar v1.AdmissionReview) *v1.AdmissionResponse {
	raw := ar.Request.Object.Raw
	pod := corev1.Pod{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
		log.Errorf("can not decode into pod, err: %v, raw: %s", err, string(raw))
		return toV1AdmissionResponse(err)
	}

	from, err := json.Marshal(pod)
	if err != nil {
		log.Errorf("can not marshal into pod, err: %v", err)
		return toV1AdmissionResponse(err)
	}

	// 1) pre-check
	container, index := util.FindContainerByName(&pod, config.ContainerSidecarVPN)
	if container == nil {
		return &v1.AdmissionResponse{UID: ar.Request.UID, Allowed: true}
	}
	value, ok := util.FindContainerEnv(container, config.EnvInboundPodTunIPv4)
	if !ok {
		return &v1.AdmissionResponse{UID: ar.Request.UID, Allowed: true}
	}
	// if create pod kubevpn-traffic-manager, just ignore it
	// because 223.254.0.100 is reserved
	if x, _, _ := net.ParseCIDR(value); config.RouterIP.Equal(x) {
		return &v1.AdmissionResponse{UID: ar.Request.UID, Allowed: true}
	}

	// 2) release old ip
	h.Lock()
	defer h.Unlock()
	cmi := h.clientset.CoreV1().ConfigMaps(ar.Request.Namespace)
	manager := dhcp.NewDHCPManager(cmi, ar.Request.Namespace)
	var ips []net.IP
	for k := 0; k < len(container.Env); k++ {
		envVar := container.Env[k]
		if sets.New[string](config.EnvInboundPodTunIPv4, config.EnvInboundPodTunIPv6).Has(envVar.Name) && envVar.Value != "" {
			if ip, _, _ := net.ParseCIDR(envVar.Value); ip != nil {
				ips = append(ips, ip)
			}
		}
	}
	_ = manager.ReleaseIP(context.Background(), ips...)

	// 3) rent new ip
	var v4, v6 *net.IPNet
	v4, v6, err = manager.RentIP(context.Background())
	if err != nil {
		log.Errorf("rent ip random failed, err: %v", err)
		return toV1AdmissionResponse(err)
	}
	var name string
	if accessor, errT := meta.Accessor(ar.Request.Object); errT == nil {
		name = accessor.GetName()
	}
	log.Infof("rent ipv4: %s ipv6: %s for pod %s in namespace: %s", v4.String(), v6.String(), name, ar.Request.Namespace)

	//4) update spec
	for j := 0; j < len(pod.Spec.Containers[index].Env); j++ {
		pair := pod.Spec.Containers[index].Env[j]
		if pair.Name == config.EnvInboundPodTunIPv4 && v4 != nil {
			pod.Spec.Containers[index].Env[j].Value = v4.String()
		}
		if pair.Name == config.EnvInboundPodTunIPv6 && v6 != nil {
			pod.Spec.Containers[index].Env[j].Value = v6.String()
		}
	}

	// 5) generate patch and apply patch
	var to []byte
	to, err = json.Marshal(pod)
	if err != nil {
		log.Errorf("can not marshal pod, err: %v", err)
		return toV1AdmissionResponse(err)
	}
	var patch []jsonpatch.JsonPatchOperation
	patch, err = jsonpatch.CreatePatch(from, to)
	if err != nil {
		log.Errorf("can not create patch json, err: %v", err)
		return toV1AdmissionResponse(err)
	}
	var marshal []byte
	marshal, err = json.Marshal(patch)
	if err != nil {
		log.Errorf("can not marshal json patch %v, err: %v", patch, err)
		return toV1AdmissionResponse(err)
	}
	var shouldPatchPod = func(pod *corev1.Pod) bool {
		namedContainer, _ := podcmd.FindContainerByName(pod, config.ContainerSidecarVPN)
		return namedContainer != nil
	}
	return applyPodPatch(ar, shouldPatchPod, string(marshal))
}

// handle delete pod event
func (h *admissionReviewHandler) handleDelete(ar v1.AdmissionReview) *v1.AdmissionResponse {
	raw := ar.Request.OldObject.Raw
	pod := corev1.Pod{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
		log.Errorf("can not decode into pod, err: %v, raw: %s", err, string(raw))
		return toV1AdmissionResponse(err)
	}

	// 1) pre-check
	container, _ := util.FindContainerByName(&pod, config.ContainerSidecarVPN)
	if container == nil {
		return &v1.AdmissionResponse{Allowed: true}
	}
	value, ok := util.FindContainerEnv(container, config.EnvInboundPodTunIPv4)
	if !ok {
		return &v1.AdmissionResponse{Allowed: true}
	}
	// if delete pod kubevpn-traffic-manager, just ignore it
	// because 223.254.0.100 is reserved
	if x, _, _ := net.ParseCIDR(value); config.RouterIP.Equal(x) {
		return &v1.AdmissionResponse{Allowed: true}
	}

	// 2) release ip
	var ips []net.IP
	for _, envVar := range container.Env {
		if envVar.Name == config.EnvInboundPodTunIPv4 || envVar.Name == config.EnvInboundPodTunIPv6 {
			if ip, _, err := net.ParseCIDR(envVar.Value); err == nil {
				ips = append(ips, ip)
			}
		}
	}
	if len(ips) != 0 {
		h.Lock()
		defer h.Unlock()
		cmi := h.clientset.CoreV1().ConfigMaps(ar.Request.Namespace)
		err := dhcp.NewDHCPManager(cmi, ar.Request.Namespace).ReleaseIP(context.Background(), ips...)
		if err != nil {
			log.Errorf("release ip to dhcp err: %v, ips: %v", err, ips)
		} else {
			log.Debugf("release ip to dhcp ok, ip: %v", ips)
		}
	}
	return &v1.AdmissionResponse{Allowed: true}
}

func applyPodPatch(ar v1.AdmissionReview, shouldPatchPod func(*corev1.Pod) bool, patch string) *v1.AdmissionResponse {
	log.Infof("apply pod patch: %s", patch)
	podResource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	if ar.Request.Resource != podResource {
		err := fmt.Errorf("expect resource to be %s but real %s", podResource, ar.Request.Resource)
		log.Error(err)
		return toV1AdmissionResponse(err)
	}

	raw := ar.Request.Object.Raw
	pod := corev1.Pod{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
		log.Errorf("can not decode request into pod, err: %v, req: %s", err, string(raw))
		return toV1AdmissionResponse(err)
	}
	reviewResponse := v1.AdmissionResponse{Allowed: true}
	if shouldPatchPod(&pod) {
		reviewResponse.Patch = []byte(patch)
		reviewResponse.PatchType = ptr.To(v1.PatchTypeJSONPatch)
	}
	return &reviewResponse
}
