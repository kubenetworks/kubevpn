# Node-boundary Agent PoC Result — 2026-11-23

| Check | Status |
|-------|--------|
| Cluster | Minikube (GitHub Actions) |
| Image | nodeboundary-poc:latest (built locally) |
| Workflow run | https://github.com/kubenetworks/kubevpn/actions/runs/45129878854 |
| hostNetwork | false |
| TCP header match | NOT_RUN (agent pod stuck in ImagePullBackOff) |
| TCP origin forwarding | NOT_RUN (agent pod stuck in ImagePullBackOff) |
| Second workload isolation | NOT_RUN (agent pod stuck in ImagePullBackOff) |
| UDP path | NOT_RUN (agent pod stuck in ImagePullBackOff) |
| Workload UID/generation unchanged | NOT_RUN (agent pod stuck in ImagePullBackOff) |
| Decision | NO-GO (architecture issue: image loading into minikube failed) |

## Notes

- Previous failures (harness issues):
  - 45122695559: readiness check (wc -l)
  - 45123710626: GHCR tag not built
  - 45125808652: Dockerfile expects kubevpn-linux-amd64 binary
  - 45128055486: false positive (tee swallowed failure)
- This run (45129878854):
  - Agent pod stuck in ImagePullBackOff
  - Reason: image nodeboundary-poc:latest not available in minikube
  - Next steps: fix image loading into minikube
