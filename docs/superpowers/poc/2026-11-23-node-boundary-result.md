# Node-boundary Agent PoC Result — 2026-11-23

| Check | Status |
|-------|--------|
| Cluster | Minikube (GitHub Actions) |
| Image | `ghcr.io/kubenetworks/kubevpn:6f3788af10bd1f53f2d19761faf31afe14c1ec43` (built locally) |
| Workflow run | https://github.com/kubenetworks/kubevpn/actions/runs/45124261271 (queued) + https://github.com/kubenetworks/kubevpn/actions/runs/45124203311 (queued) |
| hostNetwork | false |
| TCP header match | NOT_RUN (workflows queued for >10m) |
| TCP origin forwarding | NOT_RUN (workflows queued for >10m) |
| Second workload isolation | NOT_RUN (workflows queued for >10m) |
| UDP path | NOT_RUN (workflows queued for >10m) |
| Workload UID/generation unchanged | NOT_RUN (workflows queued for >10m) |
| Decision | RETRY (GitHub Actions runner capacity issue) |

## Notes

- Multiple workflow runs triggered: 45122695559 (failed at bookinfo), 45123710626 (failed at image pull), 45124203311 (queued), 45124261271 (queued)
- The two latest runs (45124203311 and 45124261271) have been queued for more than 10 minutes
- This is a GitHub Actions runner capacity/availability issue, not a PoC architecture issue
- The PoC script (`hack/nodeboundary-poc.sh`) has been updated to build the image locally instead of pulling from GHCR
