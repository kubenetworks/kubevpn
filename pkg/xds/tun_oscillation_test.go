package xds

import (
	"context"
	"net"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// driveReconcileLoop simulates the production self-trigger: every server write to the
// ConfigMap (saveAllocs on commit) re-fires the ConfigMap informer, which re-runs
// ReconcileAllocsFromConfigMap. Here we drive that loop explicitly and auto-confirm
// any proposal (the client always accepts), recording how many commits happen and
// the sequence of committed v6 values. A healthy system converges in ≤1 commit per
// changed family; an oscillation keeps committing forever.
func driveReconcileLoop(t *testing.T, s *TunConfigServer, owner string, ch chan *rpc.TunIPResponse, maxIters int) []string {
	t.Helper()
	ctx := context.Background()
	var committedV6 []string
	for i := 0; i < maxIters; i++ {
		s.ReconcileAllocsFromConfigMap(ctx)
		// drain any pushes (watcher buffer) so it never blocks
		drained := drainPushes(ch)
		s.mu.RLock()
		p, has := s.pendingProposal[owner]
		var cand net.IP
		if has {
			if p.candV6 != nil {
				cand = p.candV6.IP
			} else if p.candV4 != nil {
				cand = p.candV4.IP
			}
		}
		s.mu.RUnlock()
		if cand == nil {
			// no pending proposal and the last push (if any) was committed → converged
			_ = drained
			return committedV6
		}
		resp, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: owner, Namespace: "test-ns", ConfirmIP: cand.String()})
		if err != nil {
			t.Fatalf("confirm %s: %v", cand, err)
		}
		if resp.IPv6 != "" {
			committedV6 = append(committedV6, resp.IPv6)
		}
	}
	return committedV6
}

func drainPushes(ch chan *rpc.TunIPResponse) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}

// TestOscillation_SingleOwnerV6ManualEdit reproduces the logged scenario: an owner
// committed at (v4=.x, v6=A); operator edits TUN_ALLOCS to keep v4 and set v6=B.
// Expected (healthy): exactly one commit (v6→B), then convergence. A bug shows the
// loop committing B, A, B, A, ... forever.
func TestOscillation_SingleOwnerV6ManualEdit(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	rA, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	v6cur := mustHostIP(t, rA.IPv6)
	v6want := freeIPs6(t, s, 1)[0]

	ch := registerWatcher(s, "A")
	// "keep v4, change v6" — the dual edit from the log (v4 first, then v6).
	setManualAllocsDual(t, s, map[string][2]string{"A": {rA.IPv4, v6want.String() + "/64"}})

	committed := driveReconcileLoop(t, s, "A", ch, 12)
	t.Logf("committed v6 sequence: %v (cur=%s want=%s)", committed, v6cur, v6want)

	if len(committed) > 1 {
		t.Fatalf("OSCILLATION: v6 committed %d times: %v", len(committed), committed)
	}
	if cv := curV6(s, "A"); !cv.Equal(v6want) {
		t.Fatalf("final v6=%s, want %s", cv, v6want)
	}
}

// TestManualIP_DualFamilyConverges is a convergence guard: operator edits BOTH v4 and
// v6 at once; confirming v4 first runs saveAllocs (which clobbers the conflated entry),
// but the v6 proposal survives in pendingProposal, so the v6 edit is NOT lost. (This
// refutes the "dual-family lost-edit" hypothesis — the lone-edit path converges.)
func TestManualIP_DualFamilyConverges(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	_, _ = s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	v4want := freeIPs(t, s, 1)[0]
	v6want := freeIPs6(t, s, 1)[0]

	ch := registerWatcher(s, "A")
	setManualAllocsDual(t, s, map[string][2]string{"A": {v4want.String() + "/16", v6want.String() + "/64"}})

	// Round 1: reconcile proposes both families.
	s.ReconcileAllocsFromConfigMap(ctx)
	drainPushes(ch)

	// Client confirms v4 first (this triggers saveAllocs).
	if _, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns", ConfirmIP: v4want.String()}); err != nil {
		t.Fatalf("confirm v4: %v", err)
	}
	// Now drive the loop: does v6 still get proposed+committed, or was it lost?
	committed := driveReconcileLoop(t, s, "A", ch, 12)
	t.Logf("post-v4-confirm committed v6 seq: %v (want v6=%s)", committed, v6want)

	if cv := curV6(s, "A"); !cv.Equal(v6want) {
		t.Fatalf("LOST EDIT: final v6=%s, want %s (operator's v6 edit was clobbered by saveAllocs)", cv, v6want)
	}
}

func restoreTunAllocs(t *testing.T, s *TunConfigServer, val string) {
	t.Helper()
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm: %v", err)
	}
	cm.Data[config.KeyTunAllocs] = val
	if _, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update cm: %v", err)
	}
}

// readTunAllocs returns the raw TUN_ALLOCS journal value.
func readTunAllocs(t *testing.T, s *TunConfigServer) string {
	t.Helper()
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm: %v", err)
	}
	return cm.Data[config.KeyTunAllocs]
}

// TestManualIP_ConvergesUnderJournalLag is the fix for the logged ::1<->::3 flap.
// TUN_ALLOCS is still both the operator's input and the server's journal output, but the
// reconcile now ignores entries whose version is older than the in-memory allocation —
// i.e. stale reads of the server's own past writes. To exercise that, before every
// reconcile after the first we restore the *pre-edit* journal snapshot (the previous
// committed value with its older version) — the exact one-write lag that used to drive
// the oscillation. With the version guard the edit is proposed once, committed once
// (which bumps the version), and every later stale read is rejected: the loop converges
// to a single commit.
func TestManualIP_ConvergesUnderJournalLag(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	rA, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	v6want := freeIPs6(t, s, 1)[0]
	ch := registerWatcher(s, "A")

	// The journal as the server wrote it before the edit — exactly what a lagging Get
	// would return (the previous committed v6 with its now-older version).
	staleJournal := readTunAllocs(t, s)
	// Operator edits v6 (keeping the version field, as `kubectl edit` does).
	setManualAllocsDual(t, s, map[string][2]string{"A": {rA.IPv4, v6want.String() + "/64"}})

	var committed []string
	for i := 0; i < 8; i++ {
		if i > 0 {
			// Model the one-write lag: the reconcile's Get returns the server's previous
			// journal write (older version) instead of the just-committed value.
			restoreTunAllocs(t, s, staleJournal)
		}
		s.ReconcileAllocsFromConfigMap(ctx)
		drainPushes(ch)
		s.mu.RLock()
		p, has := s.pendingProposal["A"]
		var cand net.IP
		if has && p.candV6 != nil {
			cand = p.candV6.IP
		}
		s.mu.RUnlock()
		if cand == nil {
			break
		}
		resp, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns", ConfirmIP: cand.String()})
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		committed = append(committed, resp.IPv6)
	}
	t.Logf("committed v6 sequence: %v", committed)
	if len(committed) != 1 {
		t.Fatalf("expected exactly one commit (converge), got %d: %v", len(committed), committed)
	}
	if cv := curV6(s, "A"); !cv.Equal(v6want) {
		t.Fatalf("final v6=%s, want %s", cv, v6want)
	}
}
