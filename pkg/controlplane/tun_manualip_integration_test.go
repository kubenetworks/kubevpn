package controlplane

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// These tests cover the dry-run manual-IP flow at the control-plane layer:
// an operator edit of TUN_ALLOCS becomes a PROPOSAL (no commit) pushed to the
// client; the client confirms (→ commit) or declines (→ drop, no rollback).

func registerWatcher(s *TunConfigServer, ownerID string) chan *rpc.TunIPResponse {
	ch := make(chan *rpc.TunIPResponse, 8)
	s.mu.Lock()
	s.watchers[ownerID] = append(s.watchers[ownerID], ch)
	s.mu.Unlock()
	return ch
}

// setManualAllocs overwrites the TUN_ALLOCS key with the given owner→CIDR map,
// simulating an operator editing the ConfigMap by hand.
func setManualAllocs(t *testing.T, s *TunConfigServer, allocs map[string]string) {
	t.Helper()
	ctx := context.Background()
	persisted := map[string]*persistedAlloc{}
	for owner, v4cidr := range allocs {
		persisted[owner] = &persistedAlloc{IPv4: v4cidr, Version: 1, LastRenew: time.Now().Unix()}
	}
	data, err := yaml.Marshal(persisted)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm: %v", err)
	}
	cm.Data[config.KeyTunAllocs] = string(data)
	if _, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update cm: %v", err)
	}
}

func ipAllocated(t *testing.T, s *TunConfigServer, ip net.IP) bool {
	t.Helper()
	found := false
	check := func(a net.IP) {
		if a.Equal(ip) {
			found = true
		}
	}
	_ = s.dhcp.ForEach(context.Background(), check, check) // check both v4 and v6
	return found
}

func waitPush(t *testing.T, ch chan *rpc.TunIPResponse) *rpc.TunIPResponse {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a push")
		return nil
	}
}

func mustHostIP(t *testing.T, cidr string) net.IP {
	t.Helper()
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("parse %q: %v", cidr, err)
	}
	return ip
}

func cidr16(ip net.IP) string { return ip.String() + "/16" }

// freeIPs returns n distinct known-free in-range IPs.
func freeIPs(t *testing.T, s *TunConfigServer, n int) []net.IP {
	t.Helper()
	ctx := context.Background()
	owners := make([]string, n)
	v4s := make([]net.IP, n)
	v6s := make([]net.IP, n)
	for i := 0; i < n; i++ {
		o := fmt.Sprintf("probe-free-%d", i)
		owners[i] = o
		r, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: o, Namespace: "test-ns"})
		if err != nil {
			t.Fatalf("probe GetTunIP: %v", err)
		}
		v4s[i] = mustHostIP(t, r.IPv4)
		if r.IPv6 != "" {
			v6s[i] = mustHostIP(t, r.IPv6)
		}
	}
	for i, o := range owners {
		if err := s.dhcp.ReleaseIP(ctx, v4s[i], v6s[i]); err != nil {
			t.Fatalf("release probe: %v", err)
		}
		s.mu.Lock()
		delete(s.allocs, o)
		s.mu.Unlock()
	}
	return v4s
}

func pendingProposalExists(s *TunConfigServer, owner string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.pendingProposal[owner]
	return ok
}

func curIP(s *TunConfigServer, owner string) net.IP {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a, ok := s.allocs[owner]; ok && a.IPv4 != nil {
		return a.IPv4.IP
	}
	return nil
}

func cmAnnotation(t *testing.T, s *TunConfigServer, key string) (string, bool) {
	t.Helper()
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm: %v", err)
	}
	v, ok := cm.Annotations[key]
	return v, ok
}

//  1. Propose does NOT commit: reconcile pushes a dry-run proposal but leaves the
//     committed allocation, bitmap, and TUN_ALLOCS actual untouched.
func TestDryRun_ProposeDoesNotCommit(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	rA, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	ip1 := mustHostIP(t, rA.IPv4)
	ip2 := freeIPs(t, s, 1)[0]

	ch := registerWatcher(s, "A")
	setManualAllocs(t, s, map[string]string{"A": cidr16(ip2)})
	s.ReconcileAllocsFromConfigMap(ctx)

	push := waitPush(t, ch)
	if !push.DryRun {
		t.Fatal("expected a DryRun proposal push")
	}
	if !mustHostIP(t, push.IPv4).Equal(ip2) {
		t.Fatalf("proposed IP=%s, want %s", push.IPv4, ip2)
	}
	if !curIP(s, "A").Equal(ip1) {
		t.Fatalf("committed alloc changed to %s, want unchanged %s", curIP(s, "A"), ip1)
	}
	if ipAllocated(t, s, ip2) {
		t.Fatal("proposed IP2 must NOT be rented before confirm")
	}
	if !pendingProposalExists(s, "A") {
		t.Fatal("pendingProposal[A] should exist after propose")
	}
}

// 2. Confirm commits exactly once.
func TestDryRun_ConfirmCommits(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	rA, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	ip1 := mustHostIP(t, rA.IPv4)
	ip2 := freeIPs(t, s, 1)[0]

	ch := registerWatcher(s, "A")
	setManualAllocs(t, s, map[string]string{"A": cidr16(ip2)})
	s.ReconcileAllocsFromConfigMap(ctx)
	waitPush(t, ch) // dry-run proposal

	resp, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns", ConfirmIP: ip2.String()})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if resp.DryRun {
		t.Fatal("confirm response must be committed (DryRun=false)")
	}
	if !mustHostIP(t, resp.IPv4).Equal(ip2) {
		t.Fatalf("committed IP=%s, want %s", resp.IPv4, ip2)
	}
	if !curIP(s, "A").Equal(ip2) {
		t.Fatalf("allocs[A]=%s, want %s", curIP(s, "A"), ip2)
	}
	if !ipAllocated(t, s, ip2) {
		t.Fatal("IP2 must be rented after confirm")
	}
	if ipAllocated(t, s, ip1) {
		t.Fatal("old IP1 must be released after confirm")
	}
	if pendingProposalExists(s, "A") {
		t.Fatal("pendingProposal[A] should be cleared after confirm")
	}
	cm, _ := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	got, _ := parsePersistedAllocs(cm.Data[config.KeyTunAllocs])
	if pa := got["A"]; pa == nil || !mustHostIP(t, pa.IPv4).Equal(ip2) {
		t.Fatalf("TUN_ALLOCS A=%+v, want %s", pa, ip2)
	}
}

// 3. Decline drops the proposal; nothing was committed, so the owner keeps its IP.
func TestDryRun_DeclineKeepsCurrent(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	rA, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	ip1 := mustHostIP(t, rA.IPv4)
	ip2 := freeIPs(t, s, 1)[0]

	ch := registerWatcher(s, "A")
	setManualAllocs(t, s, map[string]string{"A": cidr16(ip2)})
	s.ReconcileAllocsFromConfigMap(ctx)
	waitPush(t, ch)

	resp, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns", ExcludeIPs: []string{ip2.String()}})
	if err != nil {
		t.Fatalf("decline: %v", err)
	}
	if !mustHostIP(t, resp.IPv4).Equal(ip1) {
		t.Fatalf("decline should keep IP1 %s, got %s", ip1, resp.IPv4)
	}
	if !curIP(s, "A").Equal(ip1) {
		t.Fatalf("allocs[A]=%s, want unchanged %s", curIP(s, "A"), ip1)
	}
	if ipAllocated(t, s, ip2) {
		t.Fatal("declined IP2 must not be rented")
	}
	if pendingProposalExists(s, "A") {
		t.Fatal("pendingProposal[A] should be cleared after decline")
	}
	// TUN_ALLOCS reverted to actual (IP1) + annotation breadcrumb.
	deadline := time.Now().Add(2 * time.Second)
	for {
		cm, _ := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		got, _ := parsePersistedAllocs(cm.Data[config.KeyTunAllocs])
		_, annotated := cm.Annotations["kubevpn.io/tun-allocs-rejected"]
		if pa := got["A"]; pa != nil && mustHostIP(t, pa.IPv4).Equal(ip1) && annotated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("TUN_ALLOCS not reverted to IP1 or rejected-annotation missing")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 4. Target IP in use by another live owner → refused at propose (no push), annotated.
func TestDryRun_TargetInUseRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	rB, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "B", Namespace: "test-ns"})
	ipB := mustHostIP(t, rB.IPv4)
	aBefore := curIP(s, "A")

	ch := registerWatcher(s, "A")
	setManualAllocs(t, s, map[string]string{"A": cidr16(ipB)}) // A wants B's live IP
	s.ReconcileAllocsFromConfigMap(ctx)

	select {
	case p := <-ch:
		t.Fatalf("unexpected push for in-use target: %+v", p)
	case <-time.After(200 * time.Millisecond):
	}
	if pendingProposalExists(s, "A") {
		t.Fatal("no proposal should be created for an in-use target")
	}
	if !curIP(s, "A").Equal(aBefore) || !curIP(s, "B").Equal(ipB) {
		t.Fatal("A and B must be unchanged")
	}
	if _, ok := cmAnnotation(t, s, "kubevpn.io/tun-allocs-rejected"); !ok {
		t.Fatal("expected in-use rejection annotation")
	}
}

// 5. Out-of-range proposal is ignored.
func TestDryRun_OutOfRangeIgnored(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	ch := registerWatcher(s, "A")
	setManualAllocs(t, s, map[string]string{"A": "10.0.0.5/32"})
	s.ReconcileAllocsFromConfigMap(ctx)
	select {
	case p := <-ch:
		t.Fatalf("unexpected push for out-of-range edit: %+v", p)
	case <-time.After(200 * time.Millisecond):
	}
	if pendingProposalExists(s, "A") {
		t.Fatal("no proposal for out-of-range IP")
	}
}

// 6. Expiry: an unconfirmed proposal is dropped by the reaper; TUN_ALLOCS reverts.
func TestDryRun_ProposalExpires(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	rA, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	ip1 := mustHostIP(t, rA.IPv4)
	ip2 := freeIPs(t, s, 1)[0]

	ch := registerWatcher(s, "A")
	setManualAllocs(t, s, map[string]string{"A": cidr16(ip2)})
	s.ReconcileAllocsFromConfigMap(ctx)
	waitPush(t, ch)

	s.mu.Lock()
	p := s.pendingProposal["A"]
	p.deadline = time.Now().Add(-time.Minute)
	s.pendingProposal["A"] = p
	s.allocs["A"].LastRenew = time.Now() // keep lease fresh
	s.mu.Unlock()

	s.reapExpiredLeases(ctx)

	if pendingProposalExists(s, "A") {
		t.Fatal("expired proposal should be dropped")
	}
	if !curIP(s, "A").Equal(ip1) {
		t.Fatalf("A should be unchanged at %s, got %s", ip1, curIP(s, "A"))
	}
	if ipAllocated(t, s, ip2) {
		t.Fatal("proposed IP2 must never be rented on expiry")
	}
}

// 7. De-dup: repeated reconciles of the same desired push only one proposal.
func TestDryRun_ProposalDedup(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	ip2 := freeIPs(t, s, 1)[0]
	ch := registerWatcher(s, "A")
	setManualAllocs(t, s, map[string]string{"A": cidr16(ip2)})
	s.ReconcileAllocsFromConfigMap(ctx)
	s.ReconcileAllocsFromConfigMap(ctx)
	s.ReconcileAllocsFromConfigMap(ctx)
	waitPush(t, ch) // first proposal
	select {
	case p := <-ch:
		t.Fatalf("expected no re-push for same candidate, got %+v", p)
	case <-time.After(200 * time.Millisecond):
	}
}

// 8. Confirm when the candidate was taken since the proposal → fallback to a safe IP.
func TestDryRun_ConfirmAfterCandidateTaken(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	rA, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	ip1 := mustHostIP(t, rA.IPv4)
	ip2 := freeIPs(t, s, 1)[0]

	ch := registerWatcher(s, "A")
	setManualAllocs(t, s, map[string]string{"A": cidr16(ip2)})
	s.ReconcileAllocsFromConfigMap(ctx)
	waitPush(t, ch)

	// Someone else grabs IP2 before the client confirms.
	if err := s.dhcp.RentSpecificIP(ctx, ip2, nil); err != nil {
		t.Fatalf("occupy ip2: %v", err)
	}

	resp, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns", ConfirmIP: ip2.String(), ExcludeIPs: []string{ip1.String()}})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	got := mustHostIP(t, resp.IPv4)
	if got.Equal(ip2) {
		t.Fatal("must not commit the taken candidate IP2")
	}
	if got.Equal(ip1) {
		t.Fatal("must move off old IP1 (it was excluded)")
	}
	if !curIP(s, "A").Equal(got) || !ipAllocated(t, s, got) {
		t.Fatal("fallback IP must be committed and rented")
	}
}

//  9. Concurrent edits to different conns with different IPs → two independent
//     proposals, both confirmable; neither lost.
func TestDryRun_ConcurrentDiffConnDiffIP(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "B", Namespace: "test-ns"})
	free := freeIPs(t, s, 2)
	ipA, ipB := free[0], free[1]

	registerWatcher(s, "A")
	registerWatcher(s, "B")
	setManualAllocs(t, s, map[string]string{"A": cidr16(ipA), "B": cidr16(ipB)})
	s.ReconcileAllocsFromConfigMap(ctx)

	if !pendingProposalExists(s, "A") || !pendingProposalExists(s, "B") {
		t.Fatal("both A and B should have proposals")
	}
	if _, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns", ConfirmIP: ipA.String()}); err != nil {
		t.Fatalf("confirm A: %v", err)
	}
	if _, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "B", Namespace: "test-ns", ConfirmIP: ipB.String()}); err != nil {
		t.Fatalf("confirm B: %v", err)
	}
	if !curIP(s, "A").Equal(ipA) {
		t.Fatalf("A=%s want %s", curIP(s, "A"), ipA)
	}
	if !curIP(s, "B").Equal(ipB) {
		t.Fatalf("B=%s want %s", curIP(s, "B"), ipB)
	}
}

// ---- IPv6 / independent-family dry-run tests ----

// freeIPs6 returns n distinct known-free in-range IPv6 addresses.
func freeIPs6(t *testing.T, s *TunConfigServer, n int) []net.IP {
	t.Helper()
	ctx := context.Background()
	owners := make([]string, n)
	v4s := make([]net.IP, n)
	v6s := make([]net.IP, n)
	for i := 0; i < n; i++ {
		o := fmt.Sprintf("probe6-%d", i)
		owners[i] = o
		r, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: o, Namespace: "test-ns"})
		if err != nil {
			t.Fatalf("probe GetTunIP: %v", err)
		}
		v4s[i] = mustHostIP(t, r.IPv4)
		v6s[i] = mustHostIP(t, r.IPv6)
	}
	for i, o := range owners {
		if err := s.dhcp.ReleaseIP(ctx, v4s[i], v6s[i]); err != nil {
			t.Fatalf("release probe: %v", err)
		}
		s.mu.Lock()
		delete(s.allocs, o)
		s.mu.Unlock()
	}
	return v6s
}

// setManualAllocsDual writes owner→{ipv4cidr, ipv6cidr} (either may be "" to leave
// that family unchanged) into TUN_ALLOCS.
func setManualAllocsDual(t *testing.T, s *TunConfigServer, allocs map[string][2]string) {
	t.Helper()
	ctx := context.Background()
	persisted := map[string]*persistedAlloc{}
	for owner, pair := range allocs {
		persisted[owner] = &persistedAlloc{IPv4: pair[0], IPv6: pair[1], Version: 1, LastRenew: time.Now().Unix()}
	}
	data, err := yaml.Marshal(persisted)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm: %v", err)
	}
	cm.Data[config.KeyTunAllocs] = string(data)
	if _, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update cm: %v", err)
	}
}

func curV6(s *TunConfigServer, owner string) net.IP {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a, ok := s.allocs[owner]; ok && a.IPv6 != nil {
		return a.IPv6.IP
	}
	return nil
}

// V6-only edit: only IPv6 is proposed and committed; IPv4 is untouched.
func TestDryRun_V6OnlyConfirm(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	rA, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	v41 := mustHostIP(t, rA.IPv4)
	v61 := mustHostIP(t, rA.IPv6)
	v62 := freeIPs6(t, s, 1)[0]

	ch := registerWatcher(s, "A")
	setManualAllocsDual(t, s, map[string][2]string{"A": {rA.IPv4, v62.String() + "/64"}}) // keep v4, change v6
	s.ReconcileAllocsFromConfigMap(ctx)

	push := waitPush(t, ch)
	if !push.DryRun || push.IPv6 == "" {
		t.Fatalf("expected a v6 DryRun proposal, got %+v", push)
	}
	if push.IPv4 != "" {
		t.Fatalf("v4 must not be proposed (unchanged), got %s", push.IPv4)
	}

	resp, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns", ConfirmIP: v62.String()})
	if err != nil {
		t.Fatalf("confirm v6: %v", err)
	}
	if !mustHostIP(t, resp.IPv6).Equal(v62) {
		t.Fatalf("committed v6=%s want %s", resp.IPv6, v62)
	}
	if !mustHostIP(t, resp.IPv4).Equal(v41) {
		t.Fatalf("v4 must stay %s, got %s", v41, resp.IPv4)
	}
	if !curV6(s, "A").Equal(v62) || !curIP(s, "A").Equal(v41) {
		t.Fatalf("alloc A v4=%s v6=%s, want v4=%s v6=%s", curIP(s, "A"), curV6(s, "A"), v41, v62)
	}
	if !ipAllocated(t, s, v62) || ipAllocated(t, s, v61) || !ipAllocated(t, s, v41) {
		t.Fatal("bitmap: want v62 rented, v61 released, v41 kept")
	}
	if pendingProposalExists(s, "A") {
		t.Fatal("proposal should be cleared")
	}
}

// Both families edited at once → two proposals, confirm each independently.
func TestDryRun_BothFamiliesConfirm(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	rA, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	v41 := mustHostIP(t, rA.IPv4)
	v61 := mustHostIP(t, rA.IPv6)
	v42 := freeIPs(t, s, 1)[0]
	v62 := freeIPs6(t, s, 1)[0]

	ch := registerWatcher(s, "A")
	setManualAllocsDual(t, s, map[string][2]string{"A": {v42.String() + "/16", v62.String() + "/64"}})
	s.ReconcileAllocsFromConfigMap(ctx)
	push := waitPush(t, ch)
	if push.IPv4 == "" || push.IPv6 == "" {
		t.Fatalf("expected both families proposed, got %+v", push)
	}

	if _, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns", ConfirmIP: v42.String()}); err != nil {
		t.Fatalf("confirm v4: %v", err)
	}
	if _, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns", ConfirmIP: v62.String()}); err != nil {
		t.Fatalf("confirm v6: %v", err)
	}
	if !curIP(s, "A").Equal(v42) || !curV6(s, "A").Equal(v62) {
		t.Fatalf("A v4=%s v6=%s, want %s/%s", curIP(s, "A"), curV6(s, "A"), v42, v62)
	}
	if !ipAllocated(t, s, v42) || !ipAllocated(t, s, v62) || ipAllocated(t, s, v41) || ipAllocated(t, s, v61) {
		t.Fatal("bitmap: want v42+v62 rented, v41+v61 released")
	}
	if pendingProposalExists(s, "A") {
		t.Fatal("proposal should be cleared after both confirmed")
	}
}

// Both families edited, client confirms v4 but declines v6 → v4 commits, v6 kept.
func TestDryRun_ConfirmV4DeclineV6(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	rA, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	v61 := mustHostIP(t, rA.IPv6)
	v42 := freeIPs(t, s, 1)[0]
	v62 := freeIPs6(t, s, 1)[0]

	ch := registerWatcher(s, "A")
	setManualAllocsDual(t, s, map[string][2]string{"A": {v42.String() + "/16", v62.String() + "/64"}})
	s.ReconcileAllocsFromConfigMap(ctx)
	waitPush(t, ch)

	if _, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns", ConfirmIP: v42.String()}); err != nil {
		t.Fatalf("confirm v4: %v", err)
	}
	if _, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns", ExcludeIPs: []string{v62.String()}}); err != nil {
		t.Fatalf("decline v6: %v", err)
	}
	if !curIP(s, "A").Equal(v42) {
		t.Fatalf("v4 should commit to %s, got %s", v42, curIP(s, "A"))
	}
	if !curV6(s, "A").Equal(v61) {
		t.Fatalf("v6 should stay %s (declined), got %s", v61, curV6(s, "A"))
	}
	if ipAllocated(t, s, v62) {
		t.Fatal("declined v6 must not be rented")
	}
	if pendingProposalExists(s, "A") {
		t.Fatal("proposal should be cleared")
	}
}
