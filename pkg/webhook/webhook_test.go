package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
)

func TestServe_InvalidContentType(t *testing.T) {
	handler := admitHandler{
		v1: func(_ context.Context, _ v1.AdmissionReview) *v1.AdmissionResponse {
			return &v1.AdmissionResponse{Allowed: true}
		},
	}

	req := httptest.NewRequest("POST", "/pods", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()

	serve(w, req, handler)

	if w.Code != http.StatusOK {
		// serve returns early without writing status code for bad content type
	}
	if w.Body.Len() != 0 {
		t.Fatalf("expected empty body for invalid content type, got: %s", w.Body.String())
	}
}

func TestServe_InvalidBody(t *testing.T) {
	handler := admitHandler{
		v1: func(_ context.Context, _ v1.AdmissionReview) *v1.AdmissionResponse {
			return &v1.AdmissionResponse{Allowed: true}
		},
	}

	req := httptest.NewRequest("POST", "/pods", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	serve(w, req, handler)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid body, got %d", w.Code)
	}
}

func TestServe_ValidAdmissionReview(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	podBytes, _ := json.Marshal(pod)

	ar := v1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &v1.AdmissionRequest{
			UID:       "test-uid",
			Operation: v1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: podBytes},
		},
	}
	body, _ := json.Marshal(ar)

	called := false
	handler := admitHandler{
		v1: func(_ context.Context, review v1.AdmissionReview) *v1.AdmissionResponse {
			called = true
			if review.Request.UID != "test-uid" {
				t.Fatalf("expected UID test-uid, got %s", review.Request.UID)
			}
			return &v1.AdmissionResponse{Allowed: true, UID: review.Request.UID}
		},
	}

	req := httptest.NewRequest("POST", "/pods", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	serve(w, req, handler)

	if !called {
		t.Fatal("admit handler was not called")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp v1.AdmissionReview
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.Response.Allowed {
		t.Fatal("expected allowed=true")
	}
	if resp.Response.UID != "test-uid" {
		t.Fatalf("expected UID test-uid in response, got %s", resp.Response.UID)
	}
}

func TestServe_DryRunSkipsAdmit(t *testing.T) {
	dryRun := true
	ar := v1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &v1.AdmissionRequest{
			UID:       "dry-run-uid",
			DryRun:    &dryRun,
			Operation: v1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		},
	}
	body, _ := json.Marshal(ar)

	called := false
	handler := admitHandler{
		v1: func(_ context.Context, _ v1.AdmissionReview) *v1.AdmissionResponse {
			called = true
			return &v1.AdmissionResponse{Allowed: true}
		},
	}

	req := httptest.NewRequest("POST", "/pods", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	serve(w, req, handler)

	if called {
		t.Fatal("admit handler should NOT be called for dry run")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for dry run, got %d", w.Code)
	}

	var resp v1.AdmissionReview
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Response.Allowed {
		t.Fatal("dry run should always be allowed")
	}
}

func TestAdmitPods_WrongResource(t *testing.T) {
	h := &admissionReviewHandler{}
	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "wrong-resource-uid",
			Operation: v1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "services"},
		},
	}

	resp := h.admitPods(context.Background(), ar)

	if resp.Allowed {
		t.Fatal("expected admission to be denied for non-pod resource")
	}
	if resp.Result == nil {
		t.Fatal("expected Result to contain error message")
	}
	if resp.Result.Message == "" {
		t.Fatal("expected non-empty error message")
	}
	expectedSubstr := "expect resource to be"
	if !contains(resp.Result.Message, expectedSubstr) {
		t.Fatalf("expected error message to contain %q, got: %s", expectedSubstr, resp.Result.Message)
	}
}

func TestAdmitPods_UnsupportedOperation(t *testing.T) {
	h := &admissionReviewHandler{}
	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "update-op-uid",
			Operation: v1.Update,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		},
	}

	resp := h.admitPods(context.Background(), ar)

	if resp.Allowed {
		t.Fatal("expected admission to be denied for UPDATE operation")
	}
	if resp.Result == nil {
		t.Fatal("expected Result to contain error message")
	}
	expectedSubstr := "expect operation is"
	if !contains(resp.Result.Message, expectedSubstr) {
		t.Fatalf("expected error message to contain %q, got: %s", expectedSubstr, resp.Result.Message)
	}
}

func TestAdmitPods_ConnectOperation(t *testing.T) {
	h := &admissionReviewHandler{}
	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "connect-op-uid",
			Operation: v1.Connect,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		},
	}

	resp := h.admitPods(context.Background(), ar)

	if resp.Allowed {
		t.Fatal("expected admission to be denied for CONNECT operation")
	}
	if resp.Result == nil {
		t.Fatal("expected Result to contain error message")
	}
}

func TestApplyPodPatch_ValidPatch(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	podBytes, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("failed to marshal pod: %v", err)
	}

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "patch-uid",
			Operation: v1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: podBytes},
		},
	}

	patch := `[{"op":"add","path":"/metadata/labels","value":{"injected":"true"}}]`
	resp := applyPodPatch(context.Background(), ar, patch)

	if !resp.Allowed {
		t.Fatal("expected allowed=true for valid patch")
	}
	if resp.Patch == nil {
		t.Fatal("expected patch bytes in response")
	}
	if string(resp.Patch) != patch {
		t.Fatalf("expected patch %q, got %q", patch, string(resp.Patch))
	}
	if resp.PatchType == nil {
		t.Fatal("expected PatchType to be set")
	}
	if *resp.PatchType != v1.PatchTypeJSONPatch {
		t.Fatalf("expected PatchType JSONPatch, got %s", *resp.PatchType)
	}
}

func TestApplyPodPatch_WrongResource(t *testing.T) {
	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "patch-wrong-resource-uid",
			Operation: v1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
			Object:    runtime.RawExtension{Raw: []byte("{}")},
		},
	}

	resp := applyPodPatch(context.Background(), ar, `[]`)

	if resp.Allowed {
		t.Fatal("expected denied for non-pod resource in applyPodPatch")
	}
	if resp.Result == nil || resp.Result.Message == "" {
		t.Fatal("expected error message for wrong resource")
	}
}

func TestApplyPodPatch_InvalidPodObject(t *testing.T) {
	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "patch-invalid-pod-uid",
			Operation: v1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: []byte("not valid pod json")},
		},
	}

	resp := applyPodPatch(context.Background(), ar, `[]`)

	if resp.Allowed {
		t.Fatal("expected denied for invalid pod object")
	}
	if resp.Result == nil || resp.Result.Message == "" {
		t.Fatal("expected error message for decode failure")
	}
}

func TestApplyPodPatch_EmptyPatch(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-patch-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	podBytes, _ := json.Marshal(pod)

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "empty-patch-uid",
			Operation: v1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: podBytes},
		},
	}

	resp := applyPodPatch(context.Background(), ar, `[]`)

	if !resp.Allowed {
		t.Fatal("expected allowed=true for empty patch")
	}
	if string(resp.Patch) != "[]" {
		t.Fatalf("expected empty patch array, got %q", string(resp.Patch))
	}
}

func TestToV1AdmissionResponse(t *testing.T) {
	err := fmt.Errorf("something went wrong")
	resp := toV1AdmissionResponse(err)

	if resp.Allowed {
		t.Fatal("expected Allowed=false")
	}
	if resp.Result == nil {
		t.Fatal("expected Result to be set")
	}
	if resp.Result.Message != "something went wrong" {
		t.Fatalf("expected error message 'something went wrong', got %q", resp.Result.Message)
	}
}

func TestHandleCreate_NoPodVPN(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "no-vpn-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
			},
		},
	}
	podBytes, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("failed to marshal pod: %v", err)
	}

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "no-vpn-create-uid",
			Operation: v1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: podBytes},
		},
	}

	h := &admissionReviewHandler{}
	resp := h.handleCreate(context.Background(), ar)

	if !resp.Allowed {
		t.Fatal("expected Allowed=true when pod has no vpn sidecar")
	}
	if resp.Patch != nil {
		t.Fatalf("expected no patch, got: %s", string(resp.Patch))
	}
}

func TestHandleCreate_NoEnvVar(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-no-env-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
				{Name: "vpn", Image: "kubevpn:latest", Env: []corev1.EnvVar{
					{Name: "SOME_OTHER_ENV", Value: "value"},
				}},
			},
		},
	}
	podBytes, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("failed to marshal pod: %v", err)
	}

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "no-env-create-uid",
			Operation: v1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: podBytes},
		},
	}

	h := &admissionReviewHandler{}
	resp := h.handleCreate(context.Background(), ar)

	if !resp.Allowed {
		t.Fatal("expected Allowed=true when vpn container has no TunIPv4 env var")
	}
	if resp.Patch != nil {
		t.Fatalf("expected no patch, got: %s", string(resp.Patch))
	}
}

func TestHandleDelete_NoPodVPN(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "no-vpn-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
			},
		},
	}
	podBytes, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("failed to marshal pod: %v", err)
	}

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "no-vpn-delete-uid",
			Operation: v1.Delete,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			OldObject: runtime.RawExtension{Raw: podBytes},
		},
	}

	h := &admissionReviewHandler{}
	resp := h.handleDelete(context.Background(), ar)

	if !resp.Allowed {
		t.Fatal("expected Allowed=true when pod has no vpn sidecar on delete")
	}
}

func TestHandleDelete_WithVPNButNoIP(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-empty-ip-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
				{Name: "vpn", Image: "kubevpn:latest", Env: []corev1.EnvVar{
					{Name: "TunIPv4", Value: ""},
					{Name: "TunIPv6", Value: ""},
				}},
			},
		},
	}
	podBytes, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("failed to marshal pod: %v", err)
	}

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "vpn-empty-ip-delete-uid",
			Operation: v1.Delete,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			OldObject: runtime.RawExtension{Raw: podBytes},
		},
	}

	h := &admissionReviewHandler{}
	resp := h.handleDelete(context.Background(), ar)

	if !resp.Allowed {
		t.Fatal("expected Allowed=true when vpn container has empty IP env values")
	}
}

func newTestHandler(t *testing.T) *admissionReviewHandler {
	t.Helper()
	clientset := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: types.UID("uid-123456789012")}},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyDHCP: "", config.KeyDHCP6: ""},
		},
	)
	mgr := dhcp.NewDHCPManager(clientset, "test-ns")
	if err := mgr.InitDHCP(context.Background()); err != nil {
		t.Fatalf("InitDHCP: %v", err)
	}
	return &admissionReviewHandler{dhcp: mgr}
}

func TestHandleCreate_WithVPN_AllocatesIP(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-pod", Namespace: "test-ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
				{
					Name:  config.ContainerSidecarVPN,
					Image: "kubevpn:latest",
					Env: []corev1.EnvVar{
						{Name: config.EnvInboundPodTunIPv4, Value: ""},
						{Name: config.EnvInboundPodTunIPv6, Value: ""},
					},
				},
			},
		},
	}
	podBytes, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("failed to marshal pod: %v", err)
	}

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "create-vpn-uid",
			Operation: v1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: podBytes},
		},
	}

	resp := h.handleCreate(ctx, ar)

	if !resp.Allowed {
		msg := ""
		if resp.Result != nil {
			msg = resp.Result.Message
		}
		t.Fatalf("expected Allowed=true, got false: %s", msg)
	}
	if resp.Patch == nil {
		t.Fatal("expected a JSON patch with allocated IPs, got nil")
	}

	// Apply the patch operations to the original pod to verify IP allocation
	var ops []map[string]interface{}
	if err := json.Unmarshal(resp.Patch, &ops); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}
	if len(ops) == 0 {
		t.Fatal("expected non-empty patch operations")
	}

	// Verify the patch contains env value replacements with valid CIDRs
	foundIPv4 := false
	foundIPv6 := false
	for _, op := range ops {
		path, _ := op["path"].(string)
		value, _ := op["value"].(string)
		if value == "" {
			continue
		}
		ip, ipNet, err := net.ParseCIDR(value)
		if err != nil {
			continue
		}
		if ip == nil || ipNet == nil {
			continue
		}
		// Check if the patch path points to a container env value
		if len(path) > 0 {
			if ip.To4() != nil {
				foundIPv4 = true
				if !config.CIDR.Contains(ip) {
					t.Errorf("allocated IPv4 %s not in CIDR %s", ip, config.CIDR)
				}
			} else {
				foundIPv6 = true
				if !config.CIDR6.Contains(ip) {
					t.Errorf("allocated IPv6 %s not in CIDR %s", ip, config.CIDR6)
				}
			}
		}
	}
	if !foundIPv4 {
		t.Errorf("patch did not contain an IPv4 CIDR allocation, patch: %s", string(resp.Patch))
	}
	if !foundIPv6 {
		t.Errorf("patch did not contain an IPv6 CIDR allocation, patch: %s", string(resp.Patch))
	}
}

func TestHandleDelete_WithVPN_ReleasesIP(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	// First, rent an IP so there is something to release
	v4, v6, err := h.dhcp.RentIP(ctx)
	if err != nil {
		t.Fatalf("RentIP failed: %v", err)
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-pod", Namespace: "test-ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
				{
					Name:  config.ContainerSidecarVPN,
					Image: "kubevpn:latest",
					Env: []corev1.EnvVar{
						{Name: config.EnvInboundPodTunIPv4, Value: v4.String()},
						{Name: config.EnvInboundPodTunIPv6, Value: v6.String()},
					},
				},
			},
		},
	}
	podBytes, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("failed to marshal pod: %v", err)
	}

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "delete-vpn-uid",
			Operation: v1.Delete,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			OldObject: runtime.RawExtension{Raw: podBytes},
		},
	}

	resp := h.handleDelete(ctx, ar)

	if !resp.Allowed {
		msg := ""
		if resp.Result != nil {
			msg = resp.Result.Message
		}
		t.Fatalf("expected Allowed=true, got false: %s", msg)
	}

	// Verify the IP was actually released by renting again — should get the same IP back
	// (since it's the only one that was allocated and then released)
	v4Again, v6Again, err := h.dhcp.RentIP(ctx)
	if err != nil {
		t.Fatalf("RentIP after release failed: %v", err)
	}
	if !v4Again.IP.Equal(v4.IP) {
		t.Errorf("expected re-rented IPv4 %s to equal released %s", v4Again.IP, v4.IP)
	}
	if !v6Again.IP.Equal(v6.IP) {
		t.Errorf("expected re-rented IPv6 %s to equal released %s", v6Again.IP, v6.IP)
	}
}

// contains checks if substr is in s.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
