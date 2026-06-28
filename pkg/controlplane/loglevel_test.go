package controlplane

import (
	"testing"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// Req3: control-plane applies config.Debug to its logger so the sidecar's
// control-plane container (deployed with --debug) logs at Debug by default.
func TestApplyDebugLevel(t *testing.T) {
	defer func(prev bool) { config.Debug = prev }(config.Debug)

	t.Run("debug on raises to DebugLevel", func(t *testing.T) {
		config.Debug = true
		l := log.New()
		l.SetLevel(log.InfoLevel)
		applyDebugLevel(log.NewEntry(l))
		if l.GetLevel() != log.DebugLevel {
			t.Fatalf("expected DebugLevel, got %v", l.GetLevel())
		}
	})

	t.Run("debug off leaves level untouched", func(t *testing.T) {
		config.Debug = false
		l := log.New()
		l.SetLevel(log.InfoLevel)
		applyDebugLevel(log.NewEntry(l))
		if l.GetLevel() != log.InfoLevel {
			t.Fatalf("expected InfoLevel unchanged, got %v", l.GetLevel())
		}
	})
}
