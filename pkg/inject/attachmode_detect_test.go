package inject

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func init() {
	// Speed up tests by reducing probe timeouts
	ProbeTimeout = 200 * time.Millisecond
	ProbePollInterval = 50 * time.Millisecond
}

func TestCheckPSALabels_Privileged(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-ns",
			Labels: map[string]string{
				psaEnforce: psaPrivileged,
			},
		},
	})

	viable, reason := checkPSALabels(context.Background(), cs, "test-ns")
	if !viable {
		t.Errorf("expected viable=true, got false (reason=%q)", reason)
	}
	if reason == "" {
		t.Errorf("expected non-empty reason")
	}
}

func TestCheckPSALabels_Restricted(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-ns",
			Labels: map[string]string{
				psaEnforce: psaRestricted,
			},
		},
	})

	viable, reason := checkPSALabels(context.Background(), cs, "test-ns")
	if viable {
		t.Errorf("expected viable=false, got true (reason=%q)", reason)
	}
	if reason == "" {
		t.Errorf("expected non-empty reason")
	}
}

func TestCheckPSALabels_Baseline(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-ns",
			Labels: map[string]string{
				psaEnforce: psaBaseline,
			},
		},
	})

	viable, reason := checkPSALabels(context.Background(), cs, "test-ns")
	if viable {
		t.Errorf("expected viable=false, got true (reason=%q)", reason)
	}
	if reason == "" {
		t.Errorf("expected non-empty reason")
	}
}

func TestCheckPSALabels_NoLabel(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-ns",
		},
	})

	viable, reason := checkPSALabels(context.Background(), cs, "test-ns")
	if viable {
		t.Errorf("expected viable=false, got true (reason=%q)", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason, got %q", reason)
	}
}

func TestCheckPSALabels_Unknown(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-ns",
			Labels: map[string]string{
				psaEnforce: "something-else",
			},
		},
	})

	viable, reason := checkPSALabels(context.Background(), cs, "test-ns")
	if viable {
		t.Errorf("expected viable=false, got true (reason=%q)", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason, got %q", reason)
	}
}

func TestCheckPSALabels_NamespaceError(t *testing.T) {
	cs := fake.NewSimpleClientset()

	viable, reason := checkPSALabels(context.Background(), cs, "test-ns")
	if viable {
		t.Errorf("expected viable=false, got true (reason=%q)", reason)
	}
}

func TestCheckCanaryJob_CreateForbidden(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "jobs", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, errors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "test", nil)
	})

	viable, reason := checkCanaryJob(context.Background(), cs, "test-ns")
	if viable {
		t.Errorf("expected viable=false, got true (reason=%q)", reason)
	}
	if reason == "" {
		t.Errorf("expected non-empty reason")
	}
}

func TestCheckCanaryJob_PodNeverRuns(t *testing.T) {
	cs := fake.NewSimpleClientset()
	// Jobs are created but pods never show up
	viable, reason := checkCanaryJob(context.Background(), cs, "test-ns")
	// With fake clientset, the pod won't be created automatically,
	// so this times out and returns false
	if viable {
		t.Errorf("expected viable=false, got true (reason=%q)", reason)
	}
}

func TestAttachModeDetector(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-ns",
			Labels: map[string]string{
				psaEnforce: psaPrivileged,
			},
		},
	})

	detector := NewAttachModeDetector(cs, "test-ns")

	// First call runs detection
	viable, reason := detector.Detect(context.Background())
	if !viable {
		t.Errorf("expected viable=true, got false (reason=%q)", reason)
	}

	// Second call uses cache
	viable2, reason2 := detector.Detect(context.Background())
	if viable2 != viable {
		t.Errorf("expected cached viable=%v, got %v", viable, viable2)
	}
	if reason2 != reason {
		t.Errorf("expected cached reason=%q, got %q", reason, reason2)
	}
}

func TestDetectNodeAgentViability_PSAFastPath(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-ns",
			Labels: map[string]string{
				psaEnforce: psaPrivileged,
			},
		},
	})

	viable, reason := DetectNodeAgentViability(context.Background(), cs, "test-ns")
	if !viable {
		t.Errorf("expected viable=true, got false (reason=%q)", reason)
	}
	if reason == "" {
		t.Errorf("expected non-empty reason")
	}
}
