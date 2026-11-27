package manager

import (
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestGenDeploySpecCarriesAttachModeEnv(t *testing.T) {
	deploy := genDeploySpec("kubevpn", config.Image, "", config.AttachModeSidecar)
	found := false
	for _, c := range deploy.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "KUBEVPN_PROXY_ATTACH_MODE" && e.Value == config.AttachModeSidecar {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("traffic-manager deploy must carry KUBEVPN_PROXY_ATTACH_MODE (default sidecar)")
	}
}

func TestGenRoleGrantsJobs(t *testing.T) {
	role := genRole("kubevpn")
	grants := false
	for _, r := range role.Rules {
		for _, g := range r.APIGroups {
			if g != "batch" {
				continue
			}
			for _, res := range r.Resources {
				if res == "jobs" {
					grants = true
				}
			}
		}
	}
	if !grants {
		t.Fatal("manager namespace Role must grant batch/jobs create/delete for node-agent Jobs")
	}
}
