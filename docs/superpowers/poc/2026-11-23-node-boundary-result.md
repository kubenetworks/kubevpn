# Node-Boundary Agent PoC Result — 2026-11-23

- Cluster: Minikube (GitHub Actions)
- Image: `ghcr.io/kubenetworks/kubevpn:6f3788af10bd1f53f2d19761faf31afe14c1ec43`
- Workflow run URL: https://github.com/kubenetworks/kubevpn/actions/runs/45123710626
- hostNetwork: false
- TCP header match: NOT_RUN (workflow failed to pull test image)
- TCP origin forwarding: NOT_RUN (workflow failed to pull test image)
- Second workload isolation: NOT_RUN (workflow failed to pull test image)
- UDP path: NOT_RUN (workflow failed to pull test image)
- Workload UID/generation unchanged: NOT_RUN (workflow failed to pull test image)
- Decision: RETRY (workflow failed at image pull step — harness issue, not architecture)

## Notes

The workflow failed at the "Pull and load test image" step:
```
Error response from daemon: failed to resolve reference "ghcr.io/kubenetworks/kubevpn:6f3788af10bd1f53f2d19761faf31afe14c1ec43": ghcr.io/kubenetworks/kubevpn:6f3788af10bd1f53f2d19761faf31afe14c1ec43: not found
```
This is a harness issue — the image with that commit hash hasn't been pushed to GHCR yet.
The PoC script `hack/nodeboundary-poc.sh` was never executed.
