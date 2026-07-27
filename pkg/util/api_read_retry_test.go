package util

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestIsTransientAPIReadErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"wrapped canceled", fmt.Errorf("list: %w", context.Canceled), false},
		{"not found", k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "x"), false},
		{"forbidden", k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errors.New("nope")), false},
		{"unauthorized", k8serrors.NewUnauthorized("bad token"), false},
		{"server timeout", k8serrors.NewServerTimeout(schema.GroupResource{Resource: "pods"}, "list", 1), true},
		{"too many requests", k8serrors.NewTooManyRequestsError("slow down"), true},
		{"service unavailable", k8serrors.NewServiceUnavailable("unavailable"), true},
		{"io.EOF", io.EOF, true},
		{"wrapped io.EOF", fmt.Errorf("stream: %w", io.EOF), true},
		{"tls handshake timeout", errors.New(`Get "https://127.0.0.1:32771/api/v1/pods": net/http: TLS handshake timeout`), true},
		{"bare EOF message", errors.New(`Get "https://127.0.0.1:49527/api/v1/pods": EOF`), true},
		{"connection reset", errors.New("read tcp 1.2.3.4:5->6.7.8.9:10: read: connection reset by peer"), true},
		{"connection refused", errors.New("dial tcp 127.0.0.1:32771: connect: connection refused"), true},
		{"permanent unknown", errors.New("some permanent parse failure"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTransientAPIReadErr(c.err); got != c.want {
				t.Fatalf("isTransientAPIReadErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// withShortConnectReadBackoff shrinks the retry backoff for the duration of a test and
// returns a restore func (call via defer) so the retry path runs in milliseconds.
func withShortConnectReadBackoff() func() {
	orig := connectReadRetryBackoff
	connectReadRetryBackoff = wait.Backoff{Steps: 5, Duration: time.Millisecond, Factor: 1.5, Jitter: 0}
	return func() { connectReadRetryBackoff = orig }
}

// TestDetectPodExists_RetriesTransientThenSucceeds: a transient EOF on the first pod List
// (an unstable/warming-up apiserver) is retried, and DetectPodExists ultimately succeeds
// instead of hard-failing the connect — the exact CI failure this hardening addresses.
func TestDetectPodExists_RetriesTransientThenSucceeds(t *testing.T) {
	defer withShortConnectReadBackoff()()

	const ns = "default"
	clientset := fake.NewSimpleClientset()
	var calls int
	clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls < 3 {
			return true, nil, errors.New(`Get "https://127.0.0.1:49527/api/v1/pods": EOF`)
		}
		return true, &corev1.PodList{Items: []corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.ConfigMapPodTrafficManager + "-abc",
				Namespace: ns,
				Labels:    map[string]string{"app": config.ConfigMapPodTrafficManager},
			},
			Status: corev1.PodStatus{
				Phase:             corev1.PodRunning,
				Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
			},
		}}}, nil
	})

	exists, err := DetectPodExists(context.Background(), clientset, ns)
	if err != nil {
		t.Fatalf("expected success after transient errors, got %v", err)
	}
	if !exists {
		t.Fatal("expected exists=true for a running traffic manager pod")
	}
	if calls != 3 {
		t.Fatalf("expected 3 List calls (2 transient + 1 success), got %d", calls)
	}
}

// TestDetectPodExists_NoRetryOnForbidden: a definitive Forbidden is surfaced immediately
// without wasting the retry budget.
func TestDetectPodExists_NoRetryOnForbidden(t *testing.T) {
	defer withShortConnectReadBackoff()()

	clientset := fake.NewSimpleClientset()
	var calls int
	clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("RBAC denied"))
	})

	_, err := DetectPodExists(context.Background(), clientset, "default")
	if !k8serrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden error back, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected no retry on Forbidden, got %d calls", calls)
	}
}
