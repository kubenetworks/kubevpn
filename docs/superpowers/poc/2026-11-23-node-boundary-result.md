# Node-boundary Agent PoC Result — 2026-11-23

| Check | Status |
|-------|--------|
| Cluster | Minikube (GitHub Actions) |
| Image | nodeboundary-poc:latest (built locally) |
| Workflow run | https://github.com/kubenetworks/kubevpn/actions/runs/45133203004 (harness issue) |
| hostNetwork | false |
| TCP header match | NOT_RUN (Envoy config parse failure) |
| TCP origin forwarding | NOT_RUN (Envoy config parse failure) |
| Second workload isolation | NOT_RUN (Envoy config parse failure) |
| UDP path | NOT_RUN (Envoy config parse failure) |
| Workload UID/generation unchanged | NOT_RUN (Envoy config parse failure) |
| Decision | RETRY (harness issue: duplicate typed_config for router filter) |

## Notes

- Multiple workflow runs triggered:
  - 45122695559: failed at bookinfo readiness (wc -l issue)
  - 45123710626: failed at image pull (GHCR tag not built)
  - 45125808652: failed at Docker build (binary filename mismatch)
  - 45128055486: **FALSE POSITIVE** (job green, but PoC script failed: agent pod never Ready)
  - 45129878854: ImagePullBackOff (imagePullPolicy=Always for :latest tried Docker Hub)
  - 45131859595: Envoy startup failure (missing typed_config for router filter)
  - 45133203004: Envoy config parse failure (duplicate typed_config for router filter)
- Run 45133203004 is a harness issue, not an architecture issue:
  - The PoC script accidentally rendered **two** `typed_config` entries for `envoy.filters.http.router`
  - This caused Envoy to fail parsing the config and never listen on port 20001
- Decision: RETRY with fixed PoC script (remove duplicate typed_config)
