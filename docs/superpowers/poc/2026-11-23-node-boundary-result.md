# Node-boundary Agent PoC Result — 2026-11-23

| Check | Status |
|-------|--------|
| Cluster | Minikube (GitHub Actions) |
| Image | nodeboundary-poc:latest (built locally) |
| Workflow run | https://github.com/kubenetworks/kubevpn/actions/runs/45131859595 (harness issue) |
| hostNetwork | false |
| TCP header match | NOT_RUN (Envoy startup failure) |
| TCP origin forwarding | NOT_RUN (Envoy startup failure) |
| Second workload isolation | NOT_RUN (Envoy startup failure) |
| UDP path | NOT_RUN (Envoy startup failure) |
| Workload UID/generation unchanged | NOT_RUN (Envoy startup failure) |
| Decision | RETRY (harness issue: missing typed_config for envoy.filters.http.router) |

## Notes

- Multiple workflow runs triggered:
  - 45122695559: failed at bookinfo readiness (wc -l issue)
  - 45123710626: failed at image pull (GHCR tag not built)
  - 45125808652: failed at Docker build (binary filename mismatch)
  - 45128055486: **FALSE POSITIVE** (job green, but PoC script failed: agent pod never Ready)
  - 45129878854: ImagePullBackOff (imagePullPolicy=Always for :latest tried Docker Hub)
  - 45131859595: Envoy startup failure (missing typed_config for router filter)
- Run 45131859595 is a harness issue, not an architecture issue:
  - Envoy failed with: "Didn't find a registered implementation for 'envoy.filters.http.router' with type URL: ''"
  - The static configs were missing the explicit typed_config for the router filter
- The PoC script has been updated to add typed_config for envoy.filters.http.router
- Decision: RETRY with updated PoC script
