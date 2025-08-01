package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/mattbaird/jsonpatch"
	"k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// create pod will rent ip and delete pod will release ip
func (h *admissionReviewHandler) admitPods(ar v1.AdmissionReview) *v1.AdmissionResponse {
	var name, ns string
	accessor, _ := meta.Accessor(ar.Request.Object.Object)
	if accessor != nil {
		name = accessor.GetName()
		ns = accessor.GetNamespace()
	}
	plog.G(context.Background()).Infof("Admitting %s pods called, name: %s, namespace: %s", ar.Request.Operation, name, ns)
	podResource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	if ar.Request.Resource != podResource {
		err := fmt.Errorf("expect resource to be %s but real %s", podResource, ar.Request.Resource)
		plog.G(context.Background()).Error(err)
		return toV1AdmissionResponse(err)
	}

	switch ar.Request.Operation {
	case v1.Create:
		return h.handleCreate(ar)

	case v1.Delete:
		return h.handleDelete(ar)

	default:
		err := fmt.Errorf("expect operation is %s or %s, not %s", v1.Create, v1.Delete, ar.Request.Operation)
		plog.G(context.Background()).Error(err)
		return toV1AdmissionResponse(err)
	}
}

// handle create pod event
func (h *admissionReviewHandler) handleCreate(ar v1.AdmissionReview) *v1.AdmissionResponse {
	raw := ar.Request.Object.Raw
	pod := corev1.Pod{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
		plog.G(context.Background()).Errorf("Failed to decode into pod, err: %v, raw: %s", err, string(raw))
		return toV1AdmissionResponse(err)
	}

	from, err := json.Marshal(pod)
	if err != nil {
		plog.G(context.Background()).Errorf("Failed to marshal into pod, err: %v", err)
		return toV1AdmissionResponse(err)
	}

	// 1) pre-check
	container, index := util.FindContainerByName(&pod, config.ContainerSidecarVPN)
	if container == nil {
		return &v1.AdmissionResponse{UID: ar.Request.UID, Allowed: true}
	}
	_, ok := util.FindContainerEnv(container, config.EnvInboundPodTunIPv4)
	if !ok {
		return &v1.AdmissionResponse{UID: ar.Request.UID, Allowed: true}
	}

	// 2) release old ip
	h.Lock()
	defer h.Unlock()
	var ipv4, ipv6 net.IP
	for k := 0; k < len(container.Env); k++ {
		envVar := container.Env[k]
		if config.EnvInboundPodTunIPv4 == envVar.Name && envVar.Value != "" {
			if ip, _, _ := net.ParseCIDR(envVar.Value); ip != nil {
				ipv4 = ip
			}
		}
		if config.EnvInboundPodTunIPv6 == envVar.Name && envVar.Value != "" {
			if ip, _, _ := net.ParseCIDR(envVar.Value); ip != nil {
				ipv6 = ip
			}
		}
	}
	_ = h.dhcp.ReleaseIP(context.Background(), ipv4, ipv6)

	// 3) rent new ip
	var v4, v6 *net.IPNet
	v4, v6, err = h.dhcp.RentIP(context.Background())
	if err != nil {
		plog.G(context.Background()).Errorf("Rent IP random failed: %v", err)
		return toV1AdmissionResponse(err)
	}
	var name string
	if accessor, errT := meta.Accessor(ar.Request.Object); errT == nil {
		name = accessor.GetName()
	}
	plog.G(context.Background()).Infof("Rent IPv4: %s IPv6: %s for pod %s in namespace: %s", v4.String(), v6.String(), name, ar.Request.Namespace)

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
		plog.G(context.Background()).Errorf("Failed to marshal pod: %v", err)
		return toV1AdmissionResponse(err)
	}
	var patch []jsonpatch.JsonPatchOperation
	patch, err = jsonpatch.CreatePatch(from, to)
	if err != nil {
		plog.G(context.Background()).Errorf("Failed to create patch json: %v", err)
		return toV1AdmissionResponse(err)
	}
	var marshal []byte
	marshal, err = json.Marshal(patch)
	if err != nil {
		plog.G(context.Background()).Errorf("Failed to marshal json patch %v, err: %v", patch, err)
		return toV1AdmissionResponse(err)
	}
	return applyPodPatch(ar, string(marshal))
}

// handle delete pod event
func (h *admissionReviewHandler) handleDelete(ar v1.AdmissionReview) *v1.AdmissionResponse {
	raw := ar.Request.OldObject.Raw
	pod := corev1.Pod{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
		plog.G(context.Background()).Errorf("Failed to decode into pod, err: %v, raw: %s", err, string(raw))
		return toV1AdmissionResponse(err)
	}

	// 1) pre-check
	container, _ := util.FindContainerByName(&pod, config.ContainerSidecarVPN)
	if container == nil {
		return &v1.AdmissionResponse{Allowed: true}
	}
	_, ok := util.FindContainerEnv(container, config.EnvInboundPodTunIPv4)
	if !ok {
		return &v1.AdmissionResponse{Allowed: true}
	}

	// 2) release ip
	var ipv4, ipv6 net.IP
	for _, envVar := range container.Env {
		if envVar.Name == config.EnvInboundPodTunIPv4 {
			if ip, _, err := net.ParseCIDR(envVar.Value); err == nil {
				ipv4 = ip
			}
		}
		if envVar.Name == config.EnvInboundPodTunIPv6 {
			if ip, _, err := net.ParseCIDR(envVar.Value); err == nil {
				ipv6 = ip
			}
		}
	}
	if ipv4 != nil || ipv6 != nil {
		h.Lock()
		defer h.Unlock()
		err := h.dhcp.ReleaseIP(context.Background(), ipv4, ipv6)
		if err != nil {
			plog.G(context.Background()).Errorf("Failed to release IPv4 %v IPv6 %s: %v", ipv4, ipv6, err)
		} else {
			plog.G(context.Background()).Debugf("Release IPv4 %v IPv6 %v", ipv4, ipv6)
		}
	}
	return &v1.AdmissionResponse{Allowed: true}
}

func applyPodPatch(ar v1.AdmissionReview, patch string) *v1.AdmissionResponse {
	plog.G(context.Background()).Infof("Apply pod patch: %s", patch)
	podResource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	if ar.Request.Resource != podResource {
		err := fmt.Errorf("expect resource to be %s but real %s", podResource, ar.Request.Resource)
		plog.G(context.Background()).Error(err)
		return toV1AdmissionResponse(err)
	}

	raw := ar.Request.Object.Raw
	pod := corev1.Pod{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
		plog.G(context.Background()).Errorf("Failed to decode request into pod, err: %v, req: %s", err, string(raw))
		return toV1AdmissionResponse(err)
	}
	reviewResponse := v1.AdmissionResponse{
		Allowed:   true,
		Patch:     []byte(patch),
		PatchType: ptr.To(v1.PatchTypeJSONPatch),
	}
	return &reviewResponse
}
