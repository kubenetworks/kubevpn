# Node-boundary Agent PoC Result — 2026-11-23

| Check | Status |
|-------|--------|
| Cluster | Minikube (GitHub Actions) |
| Image | nodeboundary-poc:latest (built locally) |
| Workflow run | https://github.com/kubenetworks/kubevpn/actions/runs/45133052768 (harness issue) |
| hostNetwork | false |
| TCP header match | NOT_RUN (wait_listener timeout bug) |
| TCP origin forwarding | NOT_RUN (wait_listener timeout bug) |
| Second workload isolation | NOT_RUN (wait_listener timeout bug) |
| UDP path | NOT_RUN (wait_listener timeout bug) |
| Workload UID/generation unchanged | NOT_RUN (wait_listener timeout bug) |
| Decision | RETRY (harness issue: wait_listener used cat instead of just opening socket) |

## Notes

- Multiple workflow runs triggered:
  - 45122695559: failed at bookinfo readiness (wc -l issue)
  - 45123710626: failed at image pull (GHCR tag not built)
  - 45125808652: failed at Docker build (binary filename mismatch)
  - 45128055486: **FALSE POSITIVE** (job green, but PoC script failed: agent pod never Ready)
  - 45129878854: ImagePullBackOff (imagePullPolicy=Always for :latest tried Docker Hub)
  - 45131859595: Envoy startup failure (missing typed_config for router filter)
  - 45133203004: Envoy config parse failure (duplicate typed_config for router filter)
  - 45133052768: wait_listener timeout bug
- Run 45133052768 is a harness issue, not an architecture issue:
  - The wait_listener function used `timeout 1 bash -c 'cat < /dev/tcp/127.0.0.1/$port'`
  - This waits for DATA on an open socket, so it returns 124 even when the listener is healthy
- The PoC script has been updated to use `timeout 1 bash -c 'exec 3<>/dev/tcp/127.0.0.1/$port'` (just open and close socket)
- Decision: RETRY with fixed PoC script
