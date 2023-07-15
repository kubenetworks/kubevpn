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

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
)

// only allow pods to pull images from specific registry.
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
		var found bool
		for i := 0; i < len(pod.Spec.Containers); i++ {
			if pod.Spec.Containers[i].Name == config.ContainerSidecarVPN {
				var v4, v6 *net.IPNet
				for j := 0; j < len(pod.Spec.Containers[i].Env); j++ {
					pair := pod.Spec.Containers[i].Env[j]
					if pair.Name == config.EnvInboundPodTunIPv4 {
						found = true
						cmi := h.clientset.CoreV1().ConfigMaps(ar.Request.Namespace)
						dhcp := handler.NewDHCPManager(cmi, ar.Request.Namespace)
						// remove old values
						if pair.Value != "" {
							var ips []net.IP
							for k := 0; k < len(pod.Spec.Containers[i].Env); k++ {
								envVar := pod.Spec.Containers[i].Env[k]
								if sets.New[string](config.EnvInboundPodTunIPv4, config.EnvInboundPodTunIPv6).Has(envVar.Name) && envVar.Value != "" {
									if ip, _, _ := net.ParseCIDR(envVar.Value); ip != nil {
										ips = append(ips, ip)
									}
								}
							}
							_ = dhcp.ReleaseIP(context.Background(), ips...)
						}
						v4, v6, err = dhcp.RentIPRandom(context.Background())
						if err != nil {
							log.Errorf("rent ip random failed, err: %v", err)
							return toV1AdmissionResponse(err)
						}
						var name string
						if accessor, errT := meta.Accessor(ar.Request.Object); errT == nil {
							name = accessor.GetName()
						}
						log.Infof("rent ipv4: %s ipv6: %s for pod %s in namespace: %s", v4.String(), v6.String(), name, ar.Request.Namespace)
					}
				}
				for j := 0; j < len(pod.Spec.Containers[i].Env); j++ {
					pair := pod.Spec.Containers[i].Env[j]
					if pair.Name == config.EnvInboundPodTunIPv4 && v4 != nil {
						pod.Spec.Containers[i].Env[j].Value = v4.String()
					}
					if pair.Name == config.EnvInboundPodTunIPv6 && v6 != nil {
						pod.Spec.Containers[i].Env[j].Value = v6.String()
					}
				}
			}
		}
		if found {
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
			return applyPodPatch(
				ar,
				func(pod *corev1.Pod) bool {
					name, _ := podcmd.FindContainerByName(pod, config.ContainerSidecarVPN)
					return name != nil
				},
				string(marshal),
			)
		}
		return &v1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: true,
		}
	case v1.Delete:
		raw := ar.Request.OldObject.Raw
		pod := corev1.Pod{}
		deserializer := codecs.UniversalDeserializer()
		if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
			log.Errorf("can not decode into pod, err: %v, raw: %s", err, string(raw))
			return toV1AdmissionResponse(err)
		}

		container, _ := podcmd.FindContainerByName(&pod, config.ContainerSidecarVPN)
		if container != nil {
			var ips []net.IP
			for _, envVar := range container.Env {
				if envVar.Name == config.EnvInboundPodTunIPv4 || envVar.Name == config.EnvInboundPodTunIPv6 {
					if ip, _, err := net.ParseCIDR(envVar.Value); err == nil {
						ips = append(ips, ip)
					}
				}
			}
			cmi := h.clientset.CoreV1().ConfigMaps(ar.Request.Namespace)
			err := handler.NewDHCPManager(cmi, ar.Request.Namespace).ReleaseIP(context.Background(), ips...)
			if err != nil {
				log.Errorf("release ip to dhcp err: %v, ips: %v", err, ips)
			} else {
				log.Errorf("release ip to dhcp ok, ip: %v", ips)
			}
		}
		return &v1.AdmissionResponse{
			Allowed: true,
		}
	default:
		err := fmt.Errorf("expect operation is %s or %s, not %s", v1.Create, v1.Delete, ar.Request.Operation)
		log.Error(err)
		return toV1AdmissionResponse(err)
	}
}

func applyPodPatch(ar v1.AdmissionReview, shouldPatchPod func(*corev1.Pod) bool, patch string) *v1.AdmissionResponse {
	r, _ := json.Marshal(ar)
	log.Infof("mutating pods called, req: %s", string(r))
	podResource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	if ar.Request.Resource != podResource {
		log.Errorf("expect resource to be %s but real is %s", podResource, ar.Request.Resource)
		return nil
	}

	raw := ar.Request.Object.Raw
	pod := corev1.Pod{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
		log.Errorf("can not decode request into pod, err: %v, req: %s", err, string(raw))
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
