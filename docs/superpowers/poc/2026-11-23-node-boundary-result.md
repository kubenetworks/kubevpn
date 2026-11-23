# Node-boundary Agent PoC Result — 2026-11-23

| Check | Status |
|-------|--------|
| Cluster | Minikube (GitHub Actions) |
| Image | nodeboundary-poc:latest (built locally) |
| Workflow run | https://github.com/kubenetworks/kubevpn/actions/runs/45129878854 (harness issue) |
| hostNetwork | false |
| TCP header match | NOT_RUN (agent pod stuck in ImagePullBackOff) |
| TCP origin forwarding | NOT_RUN (agent pod stuck in ImagePullBackOff) |
| Second workload isolation | NOT_RUN (agent pod stuck in ImagePullBackOff) |
| UDP path | NOT_RUN (agent pod stuck in ImagePullBackOff) |
| Workload UID/generation unchanged | NOT_RUN (agent pod stuck in ImagePullBackOff) |
| Decision | RETRY (harness issue: imagePullPolicy=Always for :latest) |

## Notes

- Multiple workflow runs triggered:
  - 45122695559: failed at bookinfo readiness (wc -l issue)
  - 45123710626: failed at image pull (GHCR tag not built)
  - 45125808652: failed at Docker build (binary filename mismatch)
  - 45128055486: **FALSE POSITIVE** (job green, but PoC script failed: agent pod never Ready)
  - 45129878854: ImagePullBackOff (imagePullPolicy=Always for :latest tried Docker Hub)
- Run 45129878854 is a harness issue, not an architecture issue:
  - The locally loaded image exists in minikube
  - But the pod used default imagePullPolicy=Always for :latest tag
  - So it tried to pull from docker.io/library/nodeboundary-poc:latest, which doesn't exist
- The PoC script has been updated to add imagePullPolicy: IfNotPresent
- Decision: RETRY with updated PoC script
