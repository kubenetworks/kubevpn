# Node-boundary Agent PoC Result — 2026-11-23

| Check | Status |
|-------|--------|
| Cluster | Minikube (GitHub Actions) |
| Image | nodeboundary-poc:latest (built locally) |
| Workflow run | https://github.com/kubenetworks/kubevpn/actions/runs/45134423834 (failure) |
| hostNetwork | false (confirmed in pod spec) |
| TCP header match | NOT_RUN (second Envoy instance failed to start) |
| TCP origin forwarding | NOT_RUN (second Envoy instance failed to start) |
| Second workload isolation | NOT_RUN (second Envoy instance failed to start) |
| UDP path | NOT_RUN (second Envoy instance failed to start) |
| Workload UID/generation unchanged | NOT_RUN (second Envoy instance failed to start) |
| Decision | NO_GO (harness bug: missing --base-id flag for second Envoy instance) |

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
  - 45134279801: cancelled
  - 45134165759: cancelled
  - 45134423834: second Envoy instance failed to start
- Run 45134423834 analysis:
  - First Envoy instance (reviews) started successfully on port 20001
  - Second Envoy instance (details) failed to start with error: `unable to bind domain socket with base_id=0, id=0, errno=98`
  - Root cause: Envoy requires unique `--base-id` flags when running multiple instances on the same host
  - Fix: Add `--base-id=1` for reviews and `--base-id=2` for details (and `--base-id=3` for UDP)
- hostNetwork=false was confirmed in the pod spec from the diagnostics output
- Decision: NO_GO (harness issue, not architecture issue; fix by adding unique --base-id flags)
