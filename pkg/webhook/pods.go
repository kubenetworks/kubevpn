package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/mattbaird/jsonpatch"
	"k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (h *admissionReviewHandler) admitPods(ctx context.Context, ar v1.AdmissionReview) *v1.AdmissionResponse {
	plog.G(ctx).Debugf("Admitting %s pod %s/%s", ar.Request.Operation, ar.Request.Namespace, ar.Request.Name)
	podResource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	if ar.Request.Resource != podResource {
		err := fmt.Errorf("expect resource to be %s but real %s", podResource, ar.Request.Resource)
		plog.G(ctx).Error(err)
		return toV1AdmissionResponse(err)
	}

	switch ar.Request.Operation {
	case v1.Create:
		return h.handleCreate(ctx, ar)

	case v1.Delete:
		return h.handleDelete(ctx, ar)

	default:
		err := fmt.Errorf("expect operation is %s or %s, not %s", v1.Create, v1.Delete, ar.Request.Operation)
		plog.G(ctx).Error(err)
		return toV1AdmissionResponse(err)
	}
}

func (h *admissionReviewHandler) handleCreate(ctx context.Context, ar v1.AdmissionReview) *v1.AdmissionResponse {
	raw := ar.Request.Object.Raw
	pod := corev1.Pod{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
		plog.G(ctx).Errorf("Failed to decode into pod, err: %v, raw: %s", err, string(raw))
		return toV1AdmissionResponse(err)
	}

	from, err := json.Marshal(pod)
	if err != nil {
		plog.G(ctx).Errorf("Failed to marshal into pod, err: %v", err)
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

	// 2) release old IP
	h.Lock()
	defer h.Unlock()
	dhcpCtx, dhcpCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dhcpCancel()
	ipv4, ipv6 := parseEnvIPs(container)
	_ = h.dhcp.ReleaseIP(dhcpCtx, ipv4, ipv6)

	// 3) rent new IP
	var v4, v6 *net.IPNet
	v4, v6, err = h.dhcp.RentIP(dhcpCtx)
	if err != nil {
		plog.G(ctx).Errorf("Rent IP random failed: %v", err)
		return toV1AdmissionResponse(err)
	}
	plog.G(ctx).Infof("Rent IPv4: %s IPv6: %s for pod %s/%s", v4.String(), v6.String(), ar.Request.Namespace, ar.Request.Name)

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
		plog.G(ctx).Errorf("Failed to marshal pod: %v", err)
		return toV1AdmissionResponse(err)
	}
	var patch []jsonpatch.JsonPatchOperation
	patch, err = jsonpatch.CreatePatch(from, to)
	if err != nil {
		plog.G(ctx).Errorf("Failed to create patch json: %v", err)
		return toV1AdmissionResponse(err)
	}
	var marshal []byte
	marshal, err = json.Marshal(patch)
	if err != nil {
		plog.G(ctx).Errorf("Failed to marshal json patch %v, err: %v", patch, err)
		return toV1AdmissionResponse(err)
	}
	return applyPodPatch(ctx, string(marshal))
}

func (h *admissionReviewHandler) handleDelete(ctx context.Context, ar v1.AdmissionReview) *v1.AdmissionResponse {
	raw := ar.Request.OldObject.Raw
	pod := corev1.Pod{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
		plog.G(ctx).Errorf("Failed to decode into pod, err: %v, raw: %s", err, string(raw))
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

	// 2) release IP
	ipv4, ipv6 := parseEnvIPs(container)
	if ipv4 != nil || ipv6 != nil {
		h.Lock()
		defer h.Unlock()
		dhcpCtx, dhcpCancel := context.WithTimeout(ctx, 10*time.Second)
		defer dhcpCancel()
		err := h.dhcp.ReleaseIP(dhcpCtx, ipv4, ipv6)
		if err != nil {
			plog.G(ctx).Errorf("Failed to release IPv4 %v IPv6 %s: %v", ipv4, ipv6, err)
		} else {
			plog.G(ctx).Debugf("Release IPv4 %v IPv6 %v", ipv4, ipv6)
		}
	}
	return &v1.AdmissionResponse{Allowed: true}
}

// parseEnvIPs extracts the TUN IPv4 and IPv6 addresses from the container's environment variables.
func parseEnvIPs(container *corev1.Container) (ipv4, ipv6 net.IP) {
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
	return
}

func applyPodPatch(ctx context.Context, patch string) *v1.AdmissionResponse {
	plog.G(ctx).Infof("Apply pod patch: %s", patch)
	return &v1.AdmissionResponse{
		Allowed:   true,
		Patch:     []byte(patch),
		PatchType: ptr.To(v1.PatchTypeJSONPatch),
	}
}
