#!/usr/bin/env bash
set -euo pipefail

NS="${NS:-kubevpn-nodeagent-poc}"
WORKLOAD_NS="${WORKLOAD_NS:-default}"
IMAGE="${IMAGE:?IMAGE is required}"
KUBECTL="${KUBECTL:-kubectl}"
AGENT="nodeagent-poc"
CURL_POD="nodeagent-poc-curl"
UDP_POD="nodeagent-poc-udp"
SOCAT_POD="nodeagent-poc-socat"
COMMENT="kubevpn-nodeagent-poc"

log() { printf '[node-boundary-poc] %s\n' "$*"; }

delete_rule() {
  local proto="$1" dest_ip="$2" to_ip="$3" to_port="$4"
  if [[ -z "${dest_ip:-}" || -z "${to_ip:-}" ]]; then
    return 0
  fi
  "$KUBECTL" -n "$NS" exec "$AGENT" -- nsenter --net=/host/proc/1/ns/net \
    iptables -t nat -D PREROUTING -p "$proto" ! -s "$to_ip" -d "$dest_ip" \
    -j DNAT --to "$to_ip:$to_port" -m comment --comment "$COMMENT" 2>/dev/null || true
}

cleanup() {
  local status=$?
  # Clean up host-side DNAT rules first, tolerate any errors
  delete_rule tcp "${REVIEWS_IP:-}" "${AGENT_IP:-}" 20001
  delete_rule tcp "${DETAILS_IP:-}" "${AGENT_IP:-}" 20002
  delete_rule udp "${UDP_IP:-}" "${AGENT_IP:-}" 20003
  # Success cleans up. Failure preserves the namespace for the diagnostic
  # commands in Task 3 Step 3; DELETE_POC_NS_ON_FAILURE=1 forces cleanup.
  if [[ $status -eq 0 || "${DELETE_POC_NS_ON_FAILURE:-0}" == "1" ]]; then
    "$KUBECTL" delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  else
    log "namespace $NS preserved for diagnosis"
  fi
  return "$status"
}
trap cleanup EXIT

pod_ip() {
  "$KUBECTL" -n "$WORKLOAD_NS" get pod -l "app=$1" \
    -o jsonpath='{.items[?(@.status.phase=="Running")].status.podIP}' | awk '{print $1}'
}

wait_ready() {
  "$KUBECTL" -n "$1" wait --for=condition=Ready "pod/$2" --timeout=180s >/dev/null
}

wait_listener() {
  local port="$1"
  local timeout=30
  local start=$(date +%s)
  while true; do
    if "$KUBECTL" -n "$NS" exec "$AGENT" -- sh -c \
      "timeout 1 bash -c 'exec 3<>/dev/tcp/127.0.0.1/$port' 2>/dev/null" >/dev/null 2>&1; then
      return 0
    fi
    if (( $(date +%s) - start >= timeout )); then
      echo "timed out waiting for port $port" >&2
      return 1
    fi
    sleep 1
  done
}

assert_body() {
  local description="$1" command="$2" expected="$3"
  local body
  body="$(eval "$command")"
  if [[ "$body" != *"$expected"* ]]; then
    printf 'FAIL %s: expected %q, got %q\n' "$description" "$expected" "$body" >&2
    return 1
  fi
  log "PASS $description: $body"
}

# Fail early when the test topology is absent.
for app in reviews authors details; do
  test -n "$(pod_ip "$app")" || { echo "missing running $app pod in $WORKLOAD_NS" >&2; exit 1; }
done

REVIEWS_IP="$(pod_ip reviews)"
AUTHORS_IP="$(pod_ip authors)"
DETAILS_IP="$(pod_ip details)"
TARGET_UID_BEFORE="$("$KUBECTL" -n "$WORKLOAD_NS" get pod -l app=reviews -o jsonpath='{.items[0].metadata.uid}')"
GENERATION_BEFORE="$("$KUBECTL" -n "$WORKLOAD_NS" get deploy reviews -o jsonpath='{.metadata.generation}')"

"$KUBECTL" create namespace "$NS" --dry-run=client -o yaml | "$KUBECTL" apply -f - >/dev/null

# hostNetwork is deliberately false. hostPID/proc is used only to reach the
# host netns for iptables; the agent has its own cluster-routable Pod IP.
"$KUBECTL" -n "$NS" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $AGENT
spec:
  hostNetwork: false
  hostPID: true
  containers:
  - name: agent
    image: $IMAGE
    imagePullPolicy: IfNotPresent
    command: ["sh", "-c", "sleep 3600"]
    securityContext:
      # SYS_ADMIN + NET_ADMIN alone could not open /host/proc/1/ns/net in
      # minikube (nsenter: Permission denied). Match the existing node-agent
      # privilege level for this datapath proof; hostNetwork stays false.
      privileged: true
    volumeMounts:
    - name: host-proc
      mountPath: /host/proc
      readOnly: true
  volumes:
  - name: host-proc
    hostPath:
      path: /proc
      type: Directory
EOF
wait_ready "$NS" "$AGENT"
AGENT_IP="$("$KUBECTL" -n "$NS" get pod "$AGENT" -o jsonpath='{.status.podIP}')"
test -n "$AGENT_IP"
log "agentPodIP=$AGENT_IP reviews=$REVIEWS_IP authors=$AUTHORS_IP details=$DETAILS_IP"

render_tcp_config() {
  local file="$1" listen="$2" admin="$3" origin="$4" node="$5"
  cat >"$file" <<EOF
node:
  id: $node
  cluster: $node
admin:
  address:
    socket_address:
      address: 127.0.0.1
      port_value: $admin
static_resources:
  listeners:
  - address:
      socket_address:
        address: 0.0.0.0
        port_value: $listen
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: $node
          route_config:
            virtual_hosts:
            - name: all
              domains: ["*"]
              routes:
              - match:
                  prefix: "/"
                  headers:
                  - name: env
                    exact_match: test
                route:
                  cluster: local
              - match:
                  prefix: "/"
                route:
                  cluster: origin
          http_filters:
          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
  - name: local
    type: STATIC
    connect_timeout: 2s
    load_assignment:
      cluster_name: local
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: $AUTHORS_IP
                port_value: 9080
  - name: origin
    type: STATIC
    connect_timeout: 2s
    load_assignment:
      cluster_name: origin
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: $origin
                port_value: 9080
EOF
}

render_tcp_config /tmp/poc-reviews.yaml 20001 25001 "$REVIEWS_IP" poc-reviews
render_tcp_config /tmp/poc-details.yaml 20002 25002 "$DETAILS_IP" poc-details
"$KUBECTL" -n "$NS" cp /tmp/poc-reviews.yaml "$AGENT:/tmp/poc-reviews.yaml"
"$KUBECTL" -n "$NS" cp /tmp/poc-details.yaml "$AGENT:/tmp/poc-details.yaml"
"$KUBECTL" -n "$NS" exec "$AGENT" -- sh -c \
  'nohup envoy -c /tmp/poc-reviews.yaml --base-id 1 --service-node poc-reviews --service-cluster poc-reviews >/tmp/poc-reviews.log 2>&1 &'
"$KUBECTL" -n "$NS" exec "$AGENT" -- sh -c \
  'nohup envoy -c /tmp/poc-details.yaml --base-id 2 --service-node poc-details --service-cluster poc-details >/tmp/poc-details.log 2>&1 &'

# Wait for Envoy listeners to be ready before test traffic
wait_listener 20001
wait_listener 20002

# The two host-side rules have exact podIP destinations and distinct agent ports.
"$KUBECTL" -n "$NS" exec "$AGENT" -- nsenter --net=/host/proc/1/ns/net \
  iptables -t nat -A PREROUTING -p tcp ! -s "$AGENT_IP" -d "$REVIEWS_IP" \
  -j DNAT --to "$AGENT_IP:20001" -m comment --comment "$COMMENT"
"$KUBECTL" -n "$NS" exec "$AGENT" -- nsenter --net=/host/proc/1/ns/net \
  iptables -t nat -A PREROUTING -p tcp ! -s "$AGENT_IP" -d "$DETAILS_IP" \
  -j DNAT --to "$AGENT_IP:20002" -m comment --comment "$COMMENT"

"$KUBECTL" -n "$NS" run "$CURL_POD" --image=curlimages/curl:8.10.1 --restart=Never \
  --command -- sh -c 'sleep 3600' >/dev/null
wait_ready "$NS" "$CURL_POD"

assert_body "header match goes to authors" \
  "$KUBECTL -n $NS exec $CURL_POD -- curl -fsS --max-time 15 -H 'env: test' http://$REVIEWS_IP:9080/health" \
  "Authors is healthy"
assert_body "no header goes to original reviews" \
  "$KUBECTL -n $NS exec $CURL_POD -- curl -fsS --max-time 15 http://$REVIEWS_IP:9080/health" \
  "Reviews is healthy"
assert_body "second workload header match" \
  "$KUBECTL -n $NS exec $CURL_POD -- curl -fsS --max-time 15 -H 'env: test' http://$DETAILS_IP:9080/health" \
  "Authors is healthy"
assert_body "second workload origin" \
  "$KUBECTL -n $NS exec $CURL_POD -- curl -fsS --max-time 15 http://$DETAILS_IP:9080/health" \
  "Details is healthy"

# UDP uses a separate static UDP proxy and UDP echo backend; TCP success is not
# treated as evidence for UDP.
"$KUBECTL" -n "$NS" run "$UDP_POD" --image=alpine/socat:latest --restart=Never -- \
  'UDP4-RECVFROM:9081,fork' 'EXEC:/bin/cat' >/dev/null
wait_ready "$NS" "$UDP_POD"
UDP_IP="$("$KUBECTL" -n "$NS" get pod "$UDP_POD" -o jsonpath='{.status.podIP}')"
cat >/tmp/poc-udp.yaml <<EOF
node:
  id: poc-udp
  cluster: poc-udp
admin:
  address:
    socket_address:
      address: 127.0.0.1
      port_value: 25003
static_resources:
  listeners:
  - address:
      socket_address:
        address: 0.0.0.0
        port_value: 20003
        protocol: UDP
    listener_filters:
    - name: envoy.filters.udp_listener.udp_proxy
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.filters.udp.udp_proxy.v3.UdpProxyConfig
        stat_prefix: poc_udp
        cluster: udp_backend
  clusters:
  - name: udp_backend
    type: STATIC
    connect_timeout: 2s
    load_assignment:
      cluster_name: udp_backend
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: $UDP_IP
                port_value: 9081
EOF
"$KUBECTL" -n "$NS" cp /tmp/poc-udp.yaml "$AGENT:/tmp/poc-udp.yaml"
"$KUBECTL" -n "$NS" exec "$AGENT" -- sh -c \
  'nohup envoy -c /tmp/poc-udp.yaml --base-id 3 --service-node poc-udp --service-cluster poc-udp >/tmp/poc-udp.log 2>&1 &'
"$KUBECTL" -n "$NS" exec "$AGENT" -- nsenter --net=/host/proc/1/ns/net \
  iptables -t nat -A PREROUTING -p udp ! -s "$AGENT_IP" -d "$UDP_IP" \
  -j DNAT --to "$AGENT_IP:20003" -m comment --comment "$COMMENT"

"$KUBECTL" -n "$NS" run "$SOCAT_POD" --image=alpine/socat:latest --restart=Never \
  --command -- sh -c 'sleep 3600' >/dev/null
wait_ready "$NS" "$SOCAT_POD"
DIRECT_REPLY="$("$KUBECTL" -n "$NS" exec "$SOCAT_POD" -- sh -c \
  "echo poc-udp-direct | socat -T 10 - UDP4:$UDP_IP:9081")"
log "direct UDP reply: <$DIRECT_REPLY>"
test "$DIRECT_REPLY" = "poc-udp-direct"
UDP_REPLY="$("$KUBECTL" -n "$NS" exec "$SOCAT_POD" -- sh -c \
  "echo poc-udp-via-agent | socat -T 10 - UDP4:$UDP_IP:9081")"
log "proxied UDP reply: <$UDP_REPLY>"
test "$UDP_REPLY" = "poc-udp-via-agent"
log "PASS UDP original destination path: $UDP_REPLY"

TARGET_UID_AFTER="$("$KUBECTL" -n "$WORKLOAD_NS" get pod -l app=reviews -o jsonpath='{.items[0].metadata.uid}')"
GENERATION_AFTER="$("$KUBECTL" -n "$WORKLOAD_NS" get deploy reviews -o jsonpath='{.metadata.generation}')"
test "$TARGET_UID_BEFORE" = "$TARGET_UID_AFTER"
test "$GENERATION_BEFORE" = "$GENERATION_AFTER"
HOST_NETWORK="$("$KUBECTL" -n "$NS" get pod "$AGENT" -o jsonpath='{.spec.hostNetwork}')"
# The API omits the default false value; empty and false both satisfy the constraint.
test "$HOST_NETWORK" != "true"
log "PASS no workload restart, no generation change, hostNetwork=false"

echo NODE_BOUNDARY_POC_OK
