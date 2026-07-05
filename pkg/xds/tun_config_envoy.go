package xds

import (
	"context"
	"net"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// syncEnvoyRuleIP updates Rule.LocalTunIPv4/v6 in ENVOY_CONFIG for all Rules matching ownerID.
// This triggers: Watcher → Processor → xDS push → envoy hot-update.
func (s *TunConfigServer) syncEnvoyRuleIP(ctx context.Context, ownerID string, newIPv4, newIPv6 *net.IPNet) {
	newV4Str := ""
	if newIPv4 != nil {
		newV4Str = newIPv4.IP.String()
	}
	newV6Str := ""
	if newIPv6 != nil {
		newV6Str = newIPv6.IP.String()
	}

	skipped := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return err
		}
		virtuals, parseErr := parseYaml(cm.Data[config.KeyEnvoy])
		if parseErr != nil {
			// ENVOY_CONFIG is not a valid Virtual list (empty/legacy/corrupt) — there
			// are no rules to sync. Skip rather than error: a hard failure here cannot
			// be retried into success and only spams the log.
			plog.G(ctx).Warnf("[TunConfig] syncEnvoyRuleIP: skip owner %s, cannot parse %s: %v", ownerID, config.KeyEnvoy, parseErr)
			skipped = true
			return nil
		}

		changed := false
		for _, v := range virtuals {
			for _, rule := range v.Rules {
				if rule.OwnerID == ownerID && rule.LocalTunIPv4 != newV4Str {
					rule.LocalTunIPv4 = newV4Str
					rule.LocalTunIPv6 = newV6Str
					changed = true
				}
			}
		}
		if !changed {
			return nil
		}

		data, marshalErr := yaml.Marshal(virtuals)
		if marshalErr != nil {
			return marshalErr
		}
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[config.KeyEnvoy] = string(data)
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		plog.G(ctx).Errorf("[TunConfig] syncEnvoyRuleIP failed for owner %s: %v", ownerID, err)
	} else if !skipped {
		plog.G(ctx).Infof("[TunConfig] Synced envoy rule IP for owner %s to %v", ownerID, newIPv4)
	}
}

// removeEnvoyRulesForOwner deletes all envoy rules owned by ownerID from ENVOY_CONFIG
// (dropping any Virtual left with no rules). Used by the lease reaper's abandonment pass
// to clean up a rule left behind by a client that vanished without a clean Leave (crash /
// SIGKILL). It mirrors syncEnvoyRuleIP's read-modify-write; the xDS ConfigMap watcher
// re-pushes the envoy snapshot on the change. It does NOT unpatch the sidecar container
// (that needs workload RBAC).
func (s *TunConfigServer) removeEnvoyRulesForOwner(ctx context.Context, ownerID string) {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return err
		}
		virtuals, parseErr := parseYaml(cm.Data[config.KeyEnvoy])
		if parseErr != nil {
			return nil // not a valid Virtual list (empty/legacy/corrupt): nothing to remove
		}
		changed := false
		kept := virtuals[:0]
		for _, v := range virtuals {
			rules := v.Rules[:0]
			for _, rule := range v.Rules {
				if rule.OwnerID == ownerID {
					changed = true
					continue
				}
				rules = append(rules, rule)
			}
			v.Rules = rules
			if len(v.Rules) == 0 {
				continue // drop a Virtual with no remaining rules
			}
			kept = append(kept, v)
		}
		if !changed {
			return nil
		}
		data, marshalErr := yaml.Marshal(kept)
		if marshalErr != nil {
			return marshalErr
		}
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[config.KeyEnvoy] = string(data)
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		plog.G(ctx).Warnf("[TunConfig] failed to remove envoy rules for abandoned owner %s: %v", ownerID, err)
	} else {
		plog.G(ctx).Infof("[TunConfig] removed envoy rules for abandoned owner %s (no re-acquire in %v)", ownerID, abandonmentTTL)
	}
}
