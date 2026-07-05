package inject

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds"
)

// MutationFunc is called with the current Virtual list and must return the
// updated list. Returning nil (with a nil error) signals a no-op and causes
// Mutate to skip the Update call.
type MutationFunc func(ctx context.Context, virtuals []*xds.Virtual) ([]*xds.Virtual, error)

// VirtualStore wraps a ConfigMapInterface and provides a single read-modify-write
// loop for the ENVOY_CONFIG key. All callers go through Mutate.
type VirtualStore struct {
	mapInterface v12.ConfigMapInterface
}

// NewVirtualStore creates a VirtualStore backed by the given ConfigMapInterface.
func NewVirtualStore(mapInterface v12.ConfigMapInterface) *VirtualStore {
	return &VirtualStore{mapInterface: mapInterface}
}

// Mutate performs one optimistic read-modify-write cycle with RetryOnConflict.
// It reads the ConfigMap, decodes ENVOY_CONFIG, calls fn, and writes back only
// if fn returns a non-nil updated slice.
//
// If fn returns (nil, nil) the write is skipped (idempotent no-op).
// If fn returns an error the retry loop is aborted immediately.
func (s *VirtualStore) Mutate(ctx context.Context, fn MutationFunc) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := s.mapInterface.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return err
		}
		var virtuals []*xds.Virtual
		if str, ok := cm.Data[config.KeyEnvoy]; ok && str != "" {
			if err = yaml.Unmarshal([]byte(str), &virtuals); err != nil {
				return err
			}
		}
		updated, err := fn(ctx, virtuals)
		if err != nil {
			return err
		}
		if updated == nil {
			return nil // fn signaled no-op
		}
		data, err := yaml.Marshal(updated)
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[config.KeyEnvoy] = string(data)
		_, err = s.mapInterface.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

// AddRule adds (or updates) an envoy proxy rule for the given spec.
// Delegates the read-modify-write to Mutate; preserves the logging block
// from the original addEnvoyConfig.
func (s *VirtualStore) AddRule(ctx context.Context, spec envoyRuleSpec) error {
	if err := spec.validate(); err != nil {
		return err
	}
	return s.Mutate(ctx, func(ctx context.Context, virtuals []*xds.Virtual) ([]*xds.Virtual, error) {
		updated := addVirtualRule(ctx, virtuals, spec)
		// Log the persisted rule set for this workload so the actual rules (headers/tunIP/
		// owner) are visible for diagnosing full-proxy routing and ownership.
		var rules []string
		for _, virtual := range updated {
			if virtual.UID == spec.NodeID && virtual.Namespace == spec.Namespace {
				for _, r := range virtual.Rules {
					rules = append(rules, fmt.Sprintf("{headers=%v tunV4=%q owner=%q portmap=%v}",
						r.Headers, r.LocalTunIPv4, r.OwnerID, r.PortMap))
				}
			}
		}
		plog.G(ctx).Infof("[Envoy] added rule for %s/%s (headers=%v tunV4=%q owner=%q); workload rules now=%v",
			spec.Namespace, spec.NodeID, spec.Headers, spec.LocalTunIPv4, spec.OwnerID, rules)
		return updated, nil
	})
}

// RemoveRule removes the envoy proxy rule identified by (namespace, nodeID, ownerID).
// Returns (empty, found) where empty=true means the Virtual for nodeID has no more rules.
// Callers must handle k8s IsNotFound before calling this method.
func (s *VirtualStore) RemoveRule(ctx context.Context, namespace, nodeID, ownerID string) (empty bool, found bool, err error) {
	mutateErr := s.Mutate(ctx, func(ctx context.Context, virtuals []*xds.Virtual) ([]*xds.Virtual, error) {
		// Reset per-attempt output fields so retries start clean.
		empty = false
		found = false

		for _, virtual := range virtuals {
			if nodeID == virtual.UID && namespace == virtual.Namespace {
				for i := 0; i < len(virtual.Rules); i++ {
					if ownerID == virtual.Rules[i].OwnerID {
						found = true
						virtual.Rules = append(virtual.Rules[:i], virtual.Rules[i+1:]...)
						i--
					}
				}
			}
		}
		if !found {
			// Surface ownership mismatches: a rule that cannot be removed because no rule
			// for this workload carries our ownerID is how stale rules accumulate (and how
			// an empty/IP-valued OwnerID anomaly manifests). List the owners that ARE present.
			var owners []string
			for _, virtual := range virtuals {
				if nodeID == virtual.UID && namespace == virtual.Namespace {
					for _, r := range virtual.Rules {
						owners = append(owners, fmt.Sprintf("%q(headers=%v)", r.OwnerID, r.Headers))
					}
				}
			}
			plog.G(ctx).Warnf("[Envoy] removeEnvoyConfig: ownerID %q not found for %s/%s; existing rule owners=%v",
				ownerID, namespace, nodeID, owners)
			return nil, nil // no-op: nothing changed, skip Update
		}

		// remove Virtual entries that are now empty
		for i := 0; i < len(virtuals); i++ {
			if nodeID == virtuals[i].UID && namespace == virtuals[i].Namespace && len(virtuals[i].Rules) == 0 {
				virtuals = append(virtuals[:i], virtuals[i+1:]...)
				i--
				empty = true
			}
		}
		return virtuals, nil
	})
	return empty, found, mutateErr
}
