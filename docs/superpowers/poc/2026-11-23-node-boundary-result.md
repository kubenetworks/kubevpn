# Node-boundary Agent PoC Result — 2026-11-23

| Check | Status |
|-------|--------|
| Cluster | Minikube (GitHub Actions) |
| Image | nodeboundary-poc:latest (built locally) |
| Workflow run | https://github.com/kubenetworks/kubevpn/actions/runs/45128055486 |
| hostNetwork | false |
| TCP header match | TIMEOUT (agent pod failed to become ready) |
| TCP origin forwarding | TIMEOUT |
| Second workload isolation | TIMEOUT |
| UDP path | TIMEOUT |
| Workload UID/generation unchanged | TIMEOUT |
| Decision | FAIL (agent pod readiness timeout) |

## Notes

- Previous harness issues were fixed:
  - 45122695559: readiness check (wc -l) fixed by using `app=` label correctly
  - 45123710626: GHCR tag not built — switched to building image locally
  - 45125808652: Dockerfile expects kubevpn-linux-amd64 binary — fixed by copying binary in Makefile step
- Current failure: nodeagent-poc pod timed out waiting for condition (3 minutes)
- Workflow file doesn't fail the job when hack/nodeboundary-poc.sh exits non-zero, so run shows "success" even though PoC failed
- Artifact log is uploaded and available at https://github.com/kubenetworks/kubevpn/actions/runs/45128055486/artifacts/14797071599
