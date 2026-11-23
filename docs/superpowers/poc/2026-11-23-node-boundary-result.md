# Node-Boundary Agent PoC Result — 2026-11-23

- Cluster: Minikube (GitHub Actions)
- Image: `ghcr.io/kubenetworks/kubevpn:2d1682e4f45660a2092a520ccce361b768f7f115`
- Workflow run URL: https://github.com/kubenetworks/kubevpn/actions/runs/45122695559
- hostNetwork: false
- TCP header match: NOT_RUN (workflow timed out waiting for bookinfo pods)
- TCP origin forwarding: NOT_RUN (workflow timed out waiting for bookinfo pods)
- Second workload isolation: NOT_RUN (workflow timed out waiting for bookinfo pods)
- UDP path: NOT_RUN (workflow timed out waiting for bookinfo pods)
- Workload UID/generation unchanged: NOT_RUN (workflow timed out waiting for bookinfo pods)
- Decision: RETRY (workflow failed at bookinfo pod readiness step)

## Notes

The workflow failed at the "Wait for bookinfo pods to be ready" step.
The reviews pod was ready (1/1), details pod was ready (1/1), but the readiness check script
was checking with `wc -l` which might have been counting whitespace incorrectly.
The PoC script `hack/nodeboundary-poc.sh` was never executed.
