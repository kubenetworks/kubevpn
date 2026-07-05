package handler

import (
	"context"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	applyconfignetworkingv1 "k8s.io/client-go/applyconfigurations/networking/v1"
	typednetworkingv1 "k8s.io/client-go/kubernetes/typed/networking/v1"
	"k8s.io/client-go/rest"
)

func TestNewTickerResetHandler(t *testing.T) {
	t.Run("handler functions are non-nil", func(t *testing.T) {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()

		handler := newTickerResetHandler(ticker)

		if handler.AddFunc == nil {
			t.Fatal("AddFunc should not be nil")
		}
		if handler.UpdateFunc == nil {
			t.Fatal("UpdateFunc should not be nil")
		}
		if handler.DeleteFunc == nil {
			t.Fatal("DeleteFunc should not be nil")
		}
	})

	t.Run("add event resets ticker", func(t *testing.T) {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		handler := newTickerResetHandler(ticker)

		handler.AddFunc(nil)

		select {
		case <-ticker.C:
			// ticker fired after reset, as expected
		case <-time.After(5 * time.Second):
			t.Fatal("ticker did not fire after AddFunc reset")
		}
	})

	t.Run("update event resets ticker", func(t *testing.T) {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		handler := newTickerResetHandler(ticker)

		handler.UpdateFunc(nil, nil)

		select {
		case <-ticker.C:
			// ticker fired after reset, as expected
		case <-time.After(5 * time.Second):
			t.Fatal("ticker did not fire after UpdateFunc reset")
		}
	})

	t.Run("delete event resets ticker", func(t *testing.T) {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		handler := newTickerResetHandler(ticker)

		handler.DeleteFunc(nil)

		select {
		case <-ticker.C:
			// ticker fired after reset, as expected
		case <-time.After(5 * time.Second):
			t.Fatal("ticker did not fire after DeleteFunc reset")
		}
	})
}

// fakeIngressClient implements typednetworkingv1.IngressInterface for testing.
type fakeIngressClient struct {
	items []networkingv1.Ingress
}

func (f *fakeIngressClient) List(_ context.Context, _ metav1.ListOptions) (*networkingv1.IngressList, error) {
	return &networkingv1.IngressList{Items: f.items}, nil
}

func (f *fakeIngressClient) Create(context.Context, *networkingv1.Ingress, metav1.CreateOptions) (*networkingv1.Ingress, error) {
	return nil, nil
}
func (f *fakeIngressClient) Update(context.Context, *networkingv1.Ingress, metav1.UpdateOptions) (*networkingv1.Ingress, error) {
	return nil, nil
}
func (f *fakeIngressClient) UpdateStatus(context.Context, *networkingv1.Ingress, metav1.UpdateOptions) (*networkingv1.Ingress, error) {
	return nil, nil
}
func (f *fakeIngressClient) Delete(context.Context, string, metav1.DeleteOptions) error { return nil }
func (f *fakeIngressClient) DeleteCollection(context.Context, metav1.DeleteOptions, metav1.ListOptions) error {
	return nil
}
func (f *fakeIngressClient) Get(context.Context, string, metav1.GetOptions) (*networkingv1.Ingress, error) {
	return nil, nil
}
func (f *fakeIngressClient) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	return nil, nil
}
func (f *fakeIngressClient) Patch(context.Context, string, k8stypes.PatchType, []byte, metav1.PatchOptions, ...string) (*networkingv1.Ingress, error) {
	return nil, nil
}
func (f *fakeIngressClient) Apply(context.Context, *applyconfignetworkingv1.IngressApplyConfiguration, metav1.ApplyOptions) (*networkingv1.Ingress, error) {
	return nil, nil
}
func (f *fakeIngressClient) ApplyStatus(context.Context, *applyconfignetworkingv1.IngressApplyConfiguration, metav1.ApplyOptions) (*networkingv1.Ingress, error) {
	return nil, nil
}

// fakeNetworkingV1 implements typednetworkingv1.NetworkingV1Interface for testing.
type fakeNetworkingV1 struct {
	namespaceIngresses map[string][]networkingv1.Ingress
}

func (f *fakeNetworkingV1) Ingresses(namespace string) typednetworkingv1.IngressInterface {
	return &fakeIngressClient{items: f.namespaceIngresses[namespace]}
}

func (f *fakeNetworkingV1) IngressClasses() typednetworkingv1.IngressClassInterface { return nil }
func (f *fakeNetworkingV1) NetworkPolicies(string) typednetworkingv1.NetworkPolicyInterface {
	return nil
}
func (f *fakeNetworkingV1) RESTClient() rest.Interface { return nil }

func TestGetIngressRecord(t *testing.T) {
	t.Run("no ingresses returns empty string", func(t *testing.T) {
		client := &fakeNetworkingV1{
			namespaceIngresses: map[string][]networkingv1.Ingress{
				"default": {},
			},
		}
		result := getIngressRecord(context.Background(), client, []string{"default"}, "example.com")
		if result != "" {
			t.Fatalf("expected empty string, got %q", result)
		}
	})

	t.Run("matching ingress rule returns IP", func(t *testing.T) {
		client := &fakeNetworkingV1{
			namespaceIngresses: map[string][]networkingv1.Ingress{
				"default": {
					{
						Spec: networkingv1.IngressSpec{
							Rules: []networkingv1.IngressRule{
								{Host: "example.com"},
							},
						},
						Status: networkingv1.IngressStatus{
							LoadBalancer: networkingv1.IngressLoadBalancerStatus{
								Ingress: []networkingv1.IngressLoadBalancerIngress{
									{IP: "10.0.0.1"},
								},
							},
						},
					},
				},
			},
		}
		result := getIngressRecord(context.Background(), client, []string{"default"}, "example.com")
		if result != "10.0.0.1" {
			t.Fatalf("expected 10.0.0.1, got %q", result)
		}
	})

	t.Run("non-matching domain returns empty string", func(t *testing.T) {
		client := &fakeNetworkingV1{
			namespaceIngresses: map[string][]networkingv1.Ingress{
				"default": {
					{
						Spec: networkingv1.IngressSpec{
							Rules: []networkingv1.IngressRule{
								{Host: "other.com"},
							},
						},
						Status: networkingv1.IngressStatus{
							LoadBalancer: networkingv1.IngressLoadBalancerStatus{
								Ingress: []networkingv1.IngressLoadBalancerIngress{
									{IP: "10.0.0.1"},
								},
							},
						},
					},
				},
			},
		}
		result := getIngressRecord(context.Background(), client, []string{"default"}, "example.com")
		if result != "" {
			t.Fatalf("expected empty string, got %q", result)
		}
	})

	t.Run("matching TLS host returns IP", func(t *testing.T) {
		client := &fakeNetworkingV1{
			namespaceIngresses: map[string][]networkingv1.Ingress{
				"default": {
					{
						Spec: networkingv1.IngressSpec{
							TLS: []networkingv1.IngressTLS{
								{Hosts: []string{"secure.example.com", "api.example.com"}},
							},
						},
						Status: networkingv1.IngressStatus{
							LoadBalancer: networkingv1.IngressLoadBalancerStatus{
								Ingress: []networkingv1.IngressLoadBalancerIngress{
									{IP: "10.0.0.2"},
								},
							},
						},
					},
				},
			},
		}
		result := getIngressRecord(context.Background(), client, []string{"default"}, "api.example.com")
		if result != "10.0.0.2" {
			t.Fatalf("expected 10.0.0.2, got %q", result)
		}
	})

	t.Run("multiple namespaces searched", func(t *testing.T) {
		client := &fakeNetworkingV1{
			namespaceIngresses: map[string][]networkingv1.Ingress{
				"default": {},
				"production": {
					{
						Spec: networkingv1.IngressSpec{
							Rules: []networkingv1.IngressRule{
								{Host: "prod.example.com"},
							},
						},
						Status: networkingv1.IngressStatus{
							LoadBalancer: networkingv1.IngressLoadBalancerStatus{
								Ingress: []networkingv1.IngressLoadBalancerIngress{
									{IP: "10.0.0.3"},
								},
							},
						},
					},
				},
			},
		}
		result := getIngressRecord(context.Background(), client, []string{"default", "production"}, "prod.example.com")
		if result != "10.0.0.3" {
			t.Fatalf("expected 10.0.0.3, got %q", result)
		}
	})

	t.Run("ingress without IP in status returns empty string", func(t *testing.T) {
		client := &fakeNetworkingV1{
			namespaceIngresses: map[string][]networkingv1.Ingress{
				"default": {
					{
						Spec: networkingv1.IngressSpec{
							Rules: []networkingv1.IngressRule{
								{Host: "example.com"},
							},
						},
						Status: networkingv1.IngressStatus{
							LoadBalancer: networkingv1.IngressLoadBalancerStatus{
								Ingress: []networkingv1.IngressLoadBalancerIngress{
									{Hostname: "lb.example.com"},
								},
							},
						},
					},
				},
			},
		}
		result := getIngressRecord(context.Background(), client, []string{"default"}, "example.com")
		if result != "" {
			t.Fatalf("expected empty string, got %q", result)
		}
	})

	t.Run("rule match takes precedence over TLS match", func(t *testing.T) {
		client := &fakeNetworkingV1{
			namespaceIngresses: map[string][]networkingv1.Ingress{
				"default": {
					{
						Spec: networkingv1.IngressSpec{
							Rules: []networkingv1.IngressRule{
								{Host: "example.com"},
							},
							TLS: []networkingv1.IngressTLS{
								{Hosts: []string{"example.com"}},
							},
						},
						Status: networkingv1.IngressStatus{
							LoadBalancer: networkingv1.IngressLoadBalancerStatus{
								Ingress: []networkingv1.IngressLoadBalancerIngress{
									{IP: "10.0.0.5"},
								},
							},
						},
					},
				},
			},
		}
		result := getIngressRecord(context.Background(), client, []string{"default"}, "example.com")
		if result != "10.0.0.5" {
			t.Fatalf("expected 10.0.0.5, got %q", result)
		}
	})
}
