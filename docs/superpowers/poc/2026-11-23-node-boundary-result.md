# Node-boundary Agent PoC Result — 2026-11-23

| Check | Status |
|-------|--------|
| Cluster | Minikube (GitHub Actions) |
| Image | `ghcr.io/kubenetworks/kubevpn:6f3788af10bd1f53f2d19761faf31afe14c1ec43` (built locally) |
| Workflow run | https://github.com/kubenetworks/kubevpn/actions/runs/45124261271 (cancelled) + https://github.com/kubenetworks/kubevpn/actions/runs/45124203311 (cancelled) |
| hostNetwork | false |
| TCP header match | NOT_RUN (workflows cancelled after ~21m) |
| TCP origin forwarding | NOT_RUN (workflows cancelled after ~21m) |
| Second workload isolation | NOT_RUN (workflows cancelled after ~21m) |
| UDP path | NOT_RUN (workflows cancelled after ~21m) |
| Workload UID/generation unchanged | NOT_RUN (workflows cancelled after ~21m) |
| Decision | RETRY (GitHub Actions runner timeout/cancellation) |

## Notes

- Multiple workflow runs triggered:
  - 45122695559: failed at bookinfo readiness (timeout)
  - 45123710626: failed at image pull (image not found in GHCR)
  - 45124203311: cancelled after ~21 minutes
  - 45124261271: cancelled after ~21 minutes
- Both latest runs were cancelled, likely due to GitHub Actions runner timeout or manual cancellation
- The PoC script (`hack/nodeboundary-poc.sh`) has been updated to build the image locally instead of pulling from GHCR
- This is a CI/harness issue, not a PoC architecture issue
