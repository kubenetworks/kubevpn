package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "k8s.io/api/admission/v1"
	"k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
)

// ---------------------------------------------------------------------------
// serve() tests
// ---------------------------------------------------------------------------

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

func TestServe_NilBody(t *testing.T) {
	handler := admitHandler{
		v1: func(_ context.Context, _ v1.AdmissionReview) *v1.AdmissionResponse {
			return &v1.AdmissionResponse{Allowed: true}
		},
	}

	req := httptest.NewRequest("POST", "/pods", nil)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	serve(w, req, handler)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for nil body, got %d", w.Code)
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

func TestServe_V1beta1AdmissionReview(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	podBytes, _ := json.Marshal(pod)

	ar := v1beta1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1beta1", Kind: "AdmissionReview"},
		Request: &v1beta1.AdmissionRequest{
			UID:       "v1beta1-uid",
			Operation: v1beta1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: podBytes},
		},
	}
	body, _ := json.Marshal(ar)

	called := false
	handler := newDelegateToV1AdmitHandler(func(_ context.Context, review v1.AdmissionReview) *v1.AdmissionResponse {
		called = true
		if review.Request.UID != "v1beta1-uid" {
			t.Fatalf("expected UID v1beta1-uid, got %s", review.Request.UID)
		}
		return &v1.AdmissionResponse{Allowed: true, UID: review.Request.UID}
	})

	req := httptest.NewRequest("POST", "/pods", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	serve(w, req, handler)

	if !called {
		t.Fatal("v1 admit handler was not called via v1beta1 delegation")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp v1beta1.AdmissionReview
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal v1beta1 response: %v", err)
	}
	if !resp.Response.Allowed {
		t.Fatal("expected allowed=true in v1beta1 response")
	}
	if resp.Response.UID != "v1beta1-uid" {
		t.Fatalf("expected UID v1beta1-uid, got %s", resp.Response.UID)
	}
}

func TestServe_V1beta1DryRun(t *testing.T) {
	dryRun := true
	ar := v1beta1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1beta1", Kind: "AdmissionReview"},
		Request: &v1beta1.AdmissionRequest{
			UID:       "v1beta1-dry-uid",
			DryRun:    &dryRun,
			Operation: v1beta1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		},
	}
	body, _ := json.Marshal(ar)

	called := false
	handler := newDelegateToV1AdmitHandler(func(_ context.Context, _ v1.AdmissionReview) *v1.AdmissionResponse {
		called = true
		return &v1.AdmissionResponse{Allowed: true}
	})

	req := httptest.NewRequest("POST", "/pods", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	serve(w, req, handler)

	if called {
		t.Fatal("admit handler should NOT be called for v1beta1 dry run")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for v1beta1 dry run, got %d", w.Code)
	}

	var resp v1beta1.AdmissionReview
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Response.Allowed {
		t.Fatal("v1beta1 dry run should always be allowed")
	}
	if resp.Response.UID != "v1beta1-dry-uid" {
		t.Fatalf("expected UID v1beta1-dry-uid, got %s", resp.Response.UID)
	}
}

func TestServe_UnsupportedGVK(t *testing.T) {
	pod := corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
	}
	body, _ := json.Marshal(pod)

	handler := admitHandler{
		v1: func(_ context.Context, _ v1.AdmissionReview) *v1.AdmissionResponse {
			return &v1.AdmissionResponse{Allowed: true}
		},
	}

	req := httptest.NewRequest("POST", "/pods", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	serve(w, req, handler)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported GVK, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Unsupported group version kind") {
		t.Fatalf("expected 'Unsupported group version kind' in body, got: %s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// admitPods dispatch tests
// ---------------------------------------------------------------------------

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
	if !strings.Contains(resp.Result.Message, expectedSubstr) {
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
	if !strings.Contains(resp.Result.Message, expectedSubstr) {
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

func TestAdmitPods_CreateDispatch(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "dispatch-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	podBytes, _ := json.Marshal(pod)

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "create-dispatch-uid",
			Operation: v1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: podBytes},
		},
	}

	h := &admissionReviewHandler{}
	resp := h.admitPods(context.Background(), ar)
	if !resp.Allowed {
		t.Fatal("expected Allowed=true for pod without vpn container via admitPods CREATE")
	}
}

func TestAdmitPods_DeleteDispatch(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "dispatch-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	podBytes, _ := json.Marshal(pod)

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "delete-dispatch-uid",
			Operation: v1.Delete,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			OldObject: runtime.RawExtension{Raw: podBytes},
		},
	}

	h := &admissionReviewHandler{}
	resp := h.admitPods(context.Background(), ar)
	if !resp.Allowed {
		t.Fatal("expected Allowed=true for pod without vpn container via admitPods DELETE")
	}
}

// ---------------------------------------------------------------------------
// applyPodPatch tests
// ---------------------------------------------------------------------------

func TestApplyPodPatch_ValidPatch(t *testing.T) {
	patch := `[{"op":"add","path":"/metadata/labels","value":{"injected":"true"}}]`
	resp := applyPodPatch(context.Background(), patch)

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

func TestApplyPodPatch_EmptyPatch(t *testing.T) {
	resp := applyPodPatch(context.Background(), `[]`)

	if !resp.Allowed {
		t.Fatal("expected allowed=true for empty patch")
	}
	if string(resp.Patch) != "[]" {
		t.Fatalf("expected empty patch array, got %q", string(resp.Patch))
	}
}

// ---------------------------------------------------------------------------
// toV1AdmissionResponse tests
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// handleCreate tests
// ---------------------------------------------------------------------------

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
				{Name: config.ContainerSidecarVPN, Image: "kubevpn:latest", Env: []corev1.EnvVar{
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

func TestHandleCreate_InvalidRawPod(t *testing.T) {
	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "bad-raw-uid",
			Operation: v1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: []byte("not valid pod json")},
		},
	}

	h := &admissionReviewHandler{}
	resp := h.handleCreate(context.Background(), ar)

	if resp.Allowed {
		t.Fatal("expected Allowed=false for invalid pod JSON")
	}
	if resp.Result == nil || resp.Result.Message == "" {
		t.Fatal("expected error message for invalid pod JSON")
	}
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

	var ops []map[string]any
	if err := json.Unmarshal(resp.Patch, &ops); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}
	// Patch may be empty if the re-rented IP equals the old one; that is valid.

	foundIPv4 := false
	foundIPv6 := false
	for _, op := range ops {
		value, _ := op["value"].(string)
		if value == "" {
			continue
		}
		ip, ipNet, err := net.ParseCIDR(value)
		if err != nil || ip == nil || ipNet == nil {
			continue
		}
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
	if !foundIPv4 {
		t.Errorf("patch did not contain an IPv4 CIDR allocation, patch: %s", string(resp.Patch))
	}
	if !foundIPv6 {
		t.Errorf("patch did not contain an IPv6 CIDR allocation, patch: %s", string(resp.Patch))
	}
}

func TestHandleCreate_WithPreExistingIPs(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	// Rent two IPs so the first will differ from the re-allocated one
	oldV4, oldV6, err := h.dhcp.RentIP(ctx)
	if err != nil {
		t.Fatalf("RentIP (1st) failed: %v", err)
	}
	_, _, err = h.dhcp.RentIP(ctx)
	if err != nil {
		t.Fatalf("RentIP (2nd) failed: %v", err)
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-pod-existing", Namespace: "test-ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
				{
					Name:  config.ContainerSidecarVPN,
					Image: "kubevpn:latest",
					Env: []corev1.EnvVar{
						{Name: config.EnvInboundPodTunIPv4, Value: oldV4.String()},
						{Name: config.EnvInboundPodTunIPv6, Value: oldV6.String()},
					},
				},
			},
		},
	}
	podBytes, _ := json.Marshal(pod)

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "existing-ip-create-uid",
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
		t.Fatal("expected patch with new IPs")
	}

	var ops []map[string]any
	if err := json.Unmarshal(resp.Patch, &ops); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}
	// Patch may be empty if the re-rented IP equals the old one; that is valid.
}

func TestHandleCreate_MultipleCreates_UniqueIPs(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	allocatedIPs := make(map[string]bool)
	for i := 0; i < 3; i++ {
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("vpn-pod-%d", i), Namespace: "test-ns"},
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
		podBytes, _ := json.Marshal(pod)

		ar := v1.AdmissionReview{
			Request: &v1.AdmissionRequest{
				UID:       types.UID(fmt.Sprintf("multi-create-uid-%d", i)),
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
			t.Fatalf("pod %d: expected Allowed=true, got false: %s", i, msg)
		}

		var ops []map[string]any
		if err := json.Unmarshal(resp.Patch, &ops); err != nil {
			t.Fatalf("pod %d: unmarshal patch: %v", i, err)
		}
		for _, op := range ops {
			value, _ := op["value"].(string)
			if value == "" {
				continue
			}
			if _, _, err := net.ParseCIDR(value); err != nil {
				continue
			}
			if allocatedIPs[value] {
				t.Fatalf("pod %d: duplicate IP allocation: %s", i, value)
			}
			allocatedIPs[value] = true
		}
	}

	if len(allocatedIPs) < 6 {
		t.Fatalf("expected at least 6 unique IPs (3 pods x IPv4+IPv6), got %d", len(allocatedIPs))
	}
}

func TestHandleCreate_VPNContainerIsFirst(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-first", Namespace: "test-ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  config.ContainerSidecarVPN,
					Image: "kubevpn:latest",
					Env: []corev1.EnvVar{
						{Name: config.EnvInboundPodTunIPv4, Value: ""},
						{Name: config.EnvInboundPodTunIPv6, Value: ""},
					},
				},
				{Name: "app", Image: "nginx"},
			},
		},
	}
	podBytes, _ := json.Marshal(pod)

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "vpn-first-uid",
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
		t.Fatal("expected patch when VPN container is at index 0")
	}

	var ops []map[string]any
	if err := json.Unmarshal(resp.Patch, &ops); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	for _, op := range ops {
		path, _ := op["path"].(string)
		if strings.Contains(path, "/spec/containers/") && !strings.HasPrefix(path, "/spec/containers/0/") {
			t.Fatalf("expected patch to target container index 0, got path: %s", path)
		}
	}
}

func TestHandleCreate_VPNOnlyContainer(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-only", Namespace: "test-ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
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
	podBytes, _ := json.Marshal(pod)

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "vpn-only-uid",
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
		t.Fatal("expected patch for VPN-only pod")
	}
}

func TestHandleCreate_MixedEnvVars(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "mixed-env", Namespace: "test-ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  config.ContainerSidecarVPN,
					Image: "kubevpn:latest",
					Env: []corev1.EnvVar{
						{Name: "BEFORE_ENV", Value: "before"},
						{Name: config.EnvInboundPodTunIPv4, Value: ""},
						{Name: "MIDDLE_ENV", Value: "middle"},
						{Name: config.EnvInboundPodTunIPv6, Value: ""},
						{Name: "AFTER_ENV", Value: "after"},
					},
				},
			},
		},
	}
	podBytes, _ := json.Marshal(pod)

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "mixed-env-uid",
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
		t.Fatal("expected patch for pod with mixed env vars")
	}

	var ops []map[string]any
	if err := json.Unmarshal(resp.Patch, &ops); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	for _, op := range ops {
		value, _ := op["value"].(string)
		if value == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(value); err != nil {
			t.Fatalf("patched value %q is not a valid CIDR", value)
		}
	}
}

// ---------------------------------------------------------------------------
// handleDelete tests
// ---------------------------------------------------------------------------

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

func TestHandleDelete_NoEnvVar(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-no-env", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  config.ContainerSidecarVPN,
					Image: "kubevpn:latest",
					Env:   []corev1.EnvVar{{Name: "UNRELATED", Value: "value"}},
				},
			},
		},
	}
	podBytes, _ := json.Marshal(pod)

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "delete-no-env-uid",
			Operation: v1.Delete,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			OldObject: runtime.RawExtension{Raw: podBytes},
		},
	}

	h := &admissionReviewHandler{}
	resp := h.handleDelete(context.Background(), ar)
	if !resp.Allowed {
		t.Fatal("expected Allowed=true when vpn container has no TunIPv4 env var on delete")
	}
}

func TestHandleDelete_WithVPNButNoIP(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-empty-ip-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
				{Name: config.ContainerSidecarVPN, Image: "kubevpn:latest", Env: []corev1.EnvVar{
					{Name: config.EnvInboundPodTunIPv4, Value: ""},
					{Name: config.EnvInboundPodTunIPv6, Value: ""},
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

func TestHandleDelete_InvalidRawPod(t *testing.T) {
	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "bad-raw-delete-uid",
			Operation: v1.Delete,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			OldObject: runtime.RawExtension{Raw: []byte("not valid pod json")},
		},
	}

	h := &admissionReviewHandler{}
	resp := h.handleDelete(context.Background(), ar)

	if resp.Allowed {
		t.Fatal("expected Allowed=false for invalid pod JSON on delete")
	}
	if resp.Result == nil || resp.Result.Message == "" {
		t.Fatal("expected error message for invalid pod JSON on delete")
	}
}

func TestHandleDelete_WithVPNButInvalidCIDR(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-invalid-ip", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  config.ContainerSidecarVPN,
					Image: "kubevpn:latest",
					Env: []corev1.EnvVar{
						{Name: config.EnvInboundPodTunIPv4, Value: "not-a-cidr"},
						{Name: config.EnvInboundPodTunIPv6, Value: "also-invalid"},
					},
				},
			},
		},
	}
	podBytes, _ := json.Marshal(pod)

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "delete-invalid-cidr-uid",
			Operation: v1.Delete,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			OldObject: runtime.RawExtension{Raw: podBytes},
		},
	}

	h := &admissionReviewHandler{}
	resp := h.handleDelete(context.Background(), ar)
	if !resp.Allowed {
		t.Fatal("expected Allowed=true even with invalid CIDR values on delete")
	}
}

func TestHandleDelete_WithVPN_ReleasesIP(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

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

func TestHandleDelete_OnlyIPv4Set(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	v4, _, err := h.dhcp.RentIP(ctx)
	if err != nil {
		t.Fatalf("RentIP failed: %v", err)
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-ipv4-only", Namespace: "test-ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  config.ContainerSidecarVPN,
					Image: "kubevpn:latest",
					Env: []corev1.EnvVar{
						{Name: config.EnvInboundPodTunIPv4, Value: v4.String()},
						{Name: config.EnvInboundPodTunIPv6, Value: "invalid-not-cidr"},
					},
				},
			},
		},
	}
	podBytes, _ := json.Marshal(pod)

	ar := v1.AdmissionReview{
		Request: &v1.AdmissionRequest{
			UID:       "delete-ipv4-only-uid",
			Operation: v1.Delete,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			OldObject: runtime.RawExtension{Raw: podBytes},
		},
	}

	resp := h.handleDelete(ctx, ar)
	if !resp.Allowed {
		t.Fatal("expected Allowed=true")
	}
}

// ---------------------------------------------------------------------------
// parseEnvIPs tests
// ---------------------------------------------------------------------------

func TestParseEnvIPs_ValidCIDR(t *testing.T) {
	container := &corev1.Container{
		Env: []corev1.EnvVar{
			{Name: config.EnvInboundPodTunIPv4, Value: "198.18.0.5/16"},
			{Name: config.EnvInboundPodTunIPv6, Value: "2001:2::5/64"},
		},
	}
	ipv4, ipv6 := parseEnvIPs(container)
	if ipv4 == nil {
		t.Fatal("expected non-nil IPv4")
	}
	if !ipv4.Equal(net.ParseIP("198.18.0.5")) {
		t.Fatalf("expected IPv4 198.18.0.5, got %s", ipv4)
	}
	if ipv6 == nil {
		t.Fatal("expected non-nil IPv6")
	}
	if !ipv6.Equal(net.ParseIP("2001:2::5")) {
		t.Fatalf("expected IPv6 2001:2::5, got %s", ipv6)
	}
}

func TestParseEnvIPs_InvalidCIDR(t *testing.T) {
	container := &corev1.Container{
		Env: []corev1.EnvVar{
			{Name: config.EnvInboundPodTunIPv4, Value: "not-a-cidr"},
			{Name: config.EnvInboundPodTunIPv6, Value: "also-invalid"},
		},
	}
	ipv4, ipv6 := parseEnvIPs(container)
	if ipv4 != nil {
		t.Fatalf("expected nil IPv4 for invalid CIDR, got %s", ipv4)
	}
	if ipv6 != nil {
		t.Fatalf("expected nil IPv6 for invalid CIDR, got %s", ipv6)
	}
}

func TestParseEnvIPs_EmptyValues(t *testing.T) {
	container := &corev1.Container{
		Env: []corev1.EnvVar{
			{Name: config.EnvInboundPodTunIPv4, Value: ""},
			{Name: config.EnvInboundPodTunIPv6, Value: ""},
		},
	}
	ipv4, ipv6 := parseEnvIPs(container)
	if ipv4 != nil {
		t.Fatalf("expected nil IPv4 for empty value, got %s", ipv4)
	}
	if ipv6 != nil {
		t.Fatalf("expected nil IPv6 for empty value, got %s", ipv6)
	}
}

func TestParseEnvIPs_OnlyIPv4(t *testing.T) {
	container := &corev1.Container{
		Env: []corev1.EnvVar{
			{Name: config.EnvInboundPodTunIPv4, Value: "10.0.0.1/24"},
		},
	}
	ipv4, ipv6 := parseEnvIPs(container)
	if ipv4 == nil || !ipv4.Equal(net.ParseIP("10.0.0.1")) {
		t.Fatalf("expected 10.0.0.1, got %v", ipv4)
	}
	if ipv6 != nil {
		t.Fatalf("expected nil IPv6, got %s", ipv6)
	}
}

func TestParseEnvIPs_OnlyIPv6(t *testing.T) {
	container := &corev1.Container{
		Env: []corev1.EnvVar{
			{Name: config.EnvInboundPodTunIPv6, Value: "fd00::1/128"},
		},
	}
	ipv4, ipv6 := parseEnvIPs(container)
	if ipv4 != nil {
		t.Fatalf("expected nil IPv4, got %s", ipv4)
	}
	if ipv6 == nil || !ipv6.Equal(net.ParseIP("fd00::1")) {
		t.Fatalf("expected fd00::1, got %v", ipv6)
	}
}

func TestParseEnvIPs_NoRelevantEnv(t *testing.T) {
	container := &corev1.Container{
		Env: []corev1.EnvVar{
			{Name: "UNRELATED", Value: "value"},
		},
	}
	ipv4, ipv6 := parseEnvIPs(container)
	if ipv4 != nil || ipv6 != nil {
		t.Fatalf("expected both nil, got v4=%v v6=%v", ipv4, ipv6)
	}
}

func TestParseEnvIPs_BareIPWithoutMask(t *testing.T) {
	container := &corev1.Container{
		Env: []corev1.EnvVar{
			{Name: config.EnvInboundPodTunIPv4, Value: "10.0.0.1"},
			{Name: config.EnvInboundPodTunIPv6, Value: "fd00::1"},
		},
	}
	ipv4, ipv6 := parseEnvIPs(container)
	if ipv4 != nil {
		t.Fatalf("expected nil IPv4 for bare IP without mask, got %s", ipv4)
	}
	if ipv6 != nil {
		t.Fatalf("expected nil IPv6 for bare IP without mask, got %s", ipv6)
	}
}

// ---------------------------------------------------------------------------
// isDryRun tests
// ---------------------------------------------------------------------------

func TestIsDryRun(t *testing.T) {
	trueVal := true
	falseVal := false
	tests := []struct {
		name   string
		input  *bool
		expect bool
	}{
		{"nil pointer", nil, false},
		{"true", &trueVal, true},
		{"false", &falseVal, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDryRun(tt.input)
			if got != tt.expect {
				t.Fatalf("isDryRun(%v) = %v, want %v", tt.input, got, tt.expect)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// allowedReview tests
// ---------------------------------------------------------------------------

func TestAllowedReview(t *testing.T) {
	gvk := v1.SchemeGroupVersion.WithKind("AdmissionReview")
	uid := types.UID("test-uid-123")

	review := allowedReview(&gvk, uid)

	if review.Response == nil {
		t.Fatal("expected non-nil Response")
	}
	if !review.Response.Allowed {
		t.Fatal("expected Allowed=true")
	}
	if review.Response.UID != uid {
		t.Fatalf("expected UID %s, got %s", uid, review.Response.UID)
	}
	if review.TypeMeta.Kind != "AdmissionReview" {
		t.Fatalf("expected Kind AdmissionReview, got %s", review.TypeMeta.Kind)
	}
	expectedAPI := v1.SchemeGroupVersion.String()
	if review.TypeMeta.APIVersion != expectedAPI {
		t.Fatalf("expected APIVersion %s, got %s", expectedAPI, review.TypeMeta.APIVersion)
	}
}

// ---------------------------------------------------------------------------
// convert tests
// ---------------------------------------------------------------------------

func TestConvertAdmissionRequestToV1(t *testing.T) {
	v1beta1Req := &v1beta1.AdmissionRequest{
		UID:                "convert-uid",
		Name:               "test-pod",
		Namespace:          "test-ns",
		Operation:          v1beta1.Create,
		Resource:           metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		SubResource:        "status",
		RequestSubResource: "status",
		Object:             runtime.RawExtension{Raw: []byte(`{"kind":"Pod"}`)},
		OldObject:          runtime.RawExtension{Raw: []byte(`{"kind":"Pod"}`)},
	}

	result := convertAdmissionRequestToV1(v1beta1Req)

	if result.UID != v1beta1Req.UID {
		t.Fatalf("UID: got %s, want %s", result.UID, v1beta1Req.UID)
	}
	if result.Name != v1beta1Req.Name {
		t.Fatalf("Name: got %s, want %s", result.Name, v1beta1Req.Name)
	}
	if result.Namespace != v1beta1Req.Namespace {
		t.Fatalf("Namespace: got %s, want %s", result.Namespace, v1beta1Req.Namespace)
	}
	if result.Operation != v1.Operation(v1beta1Req.Operation) {
		t.Fatalf("Operation: got %s, want %s", result.Operation, v1beta1Req.Operation)
	}
	if result.Resource != v1beta1Req.Resource {
		t.Fatalf("Resource: got %v, want %v", result.Resource, v1beta1Req.Resource)
	}
	if result.SubResource != v1beta1Req.SubResource {
		t.Fatalf("SubResource: got %s, want %s", result.SubResource, v1beta1Req.SubResource)
	}
	if result.RequestSubResource != v1beta1Req.RequestSubResource {
		t.Fatalf("RequestSubResource: got %s, want %s", result.RequestSubResource, v1beta1Req.RequestSubResource)
	}
	if string(result.Object.Raw) != string(v1beta1Req.Object.Raw) {
		t.Fatalf("Object.Raw: got %s, want %s", result.Object.Raw, v1beta1Req.Object.Raw)
	}
	if string(result.OldObject.Raw) != string(v1beta1Req.OldObject.Raw) {
		t.Fatalf("OldObject.Raw: got %s, want %s", result.OldObject.Raw, v1beta1Req.OldObject.Raw)
	}
}

func TestConvertAdmissionResponseToV1beta1(t *testing.T) {
	pt := v1.PatchTypeJSONPatch
	v1Resp := &v1.AdmissionResponse{
		UID:              "resp-uid",
		Allowed:          true,
		Patch:            []byte(`[{"op":"add","path":"/foo","value":"bar"}]`),
		PatchType:        &pt,
		AuditAnnotations: map[string]string{"key": "val"},
		Warnings:         []string{"warn1"},
		Result:           &metav1.Status{Message: "ok"},
	}

	result := convertAdmissionResponseToV1beta1(v1Resp)

	if result.UID != v1Resp.UID {
		t.Fatalf("UID: got %s, want %s", result.UID, v1Resp.UID)
	}
	if result.Allowed != v1Resp.Allowed {
		t.Fatalf("Allowed: got %v, want %v", result.Allowed, v1Resp.Allowed)
	}
	if string(result.Patch) != string(v1Resp.Patch) {
		t.Fatalf("Patch: got %s, want %s", result.Patch, v1Resp.Patch)
	}
	if result.PatchType == nil {
		t.Fatal("expected PatchType to be set")
	}
	if string(*result.PatchType) != string(*v1Resp.PatchType) {
		t.Fatalf("PatchType: got %s, want %s", *result.PatchType, *v1Resp.PatchType)
	}
	if result.AuditAnnotations["key"] != "val" {
		t.Fatal("AuditAnnotations: missing 'key' entry")
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != "warn1" {
		t.Fatalf("Warnings: got %v, want [warn1]", result.Warnings)
	}
	if result.Result == nil || result.Result.Message != "ok" {
		t.Fatal("Result not preserved")
	}
}

func TestConvertAdmissionResponseToV1beta1_NilPatchType(t *testing.T) {
	v1Resp := &v1.AdmissionResponse{
		UID:     "no-patch-uid",
		Allowed: true,
	}

	result := convertAdmissionResponseToV1beta1(v1Resp)

	if result.PatchType != nil {
		t.Fatalf("expected nil PatchType, got %v", *result.PatchType)
	}
	if !result.Allowed {
		t.Fatal("expected Allowed=true")
	}
}
