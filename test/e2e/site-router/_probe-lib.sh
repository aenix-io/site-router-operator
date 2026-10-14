# shellcheck shell=sh
# Probe and attribution helpers of the site-router suite. A script step sources
# it with `. ./_probe-lib.sh`; the working directory of a Chainsaw script is the
# directory of the suite.
#
# They drive the real topology:
#   - remote-site-b is the IPsec peer. It is driven over SSH (user admin,
#     password ${REMOTE_B_PASS}, set by its first-boot configuration), so that a
#     probe is sourced from its 192.0.2.x dummy interface into the tunnel
#     toward A.
#   - site-router-a is the gateway under test. Its firewall counters are read
#     over the management API of the appliance, with the key the operator made
#     for it, to say which guard dropped a packet: the default action of
#     TUNNEL-INGRESS, rule 1 of the input chain, the default action of forward.
#   - Drops by Cilium are attributed from the drop monitor of the agent on the
#     node of A.
#
# This is the part of the suite that knows the engine. The counters are read the
# way the barerouter appliance prints them, so another engine needs its own
# evidence here, such as a capture on the remote side.
#
# The probes run in the pod probe-driver, which has ssh, sshpass and curl. It
# lives in the namespace of the operator and carries the labels the operator is
# configured to admit to the management API of a gateway: the policy of the
# gateway lets nothing else in, so a pod of the tenant could not call it.
#
# assert_dropped takes a snapshot of the guards before the probe, so that the
# check of the guard proves the change this probe made. Every helper fails when
# its evidence is missing, so the gate cannot pass without it.

set -u
PROBE_STATE_DIR="${PROBE_STATE_DIR:-/tmp/site-router-probe}"
mkdir -p "$PROBE_STATE_DIR" 2>/dev/null || true
REMOTE_B_PASS="${REMOTE_B_PASS:-e2edrive}"
PROBE_SRC="${PROBE_SRC:-192.0.2.1}"
# Where the probe pod is, and what it is called.
PROBE_NS="${PROBE_NS:-site-router-system}"
# The key of the Secret of the gateway that holds the management key.
API_KEY_FIELD="${API_KEY_FIELD:-api-key}"

_exec() { kubectl -n "$PROBE_NS" exec -i probe-driver -- "$@"; }

# ── resolvers, cached for the shell that sources the file ────────────────────
_b_ip() {
  [ -n "${_B_IP:-}" ] && { echo "$_B_IP"; return; }
  _B_IP=$(kubectl -n "$NAMESPACE" get vmi remote-site-b \
    -o jsonpath='{.status.interfaces[0].ipAddress}' 2>/dev/null)
  echo "$_B_IP"
}
_a_pod_ip() {
  [ -n "${_A_IP:-}" ] && { echo "$_A_IP"; return; }
  _A_IP=$(kubectl -n "$NAMESPACE" get pod -l vm.kubevirt.io/name=site-router-a \
    -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)
  echo "$_A_IP"
}
_a_token() {
  [ -n "${_A_TOKEN:-}" ] && { echo "$_A_TOKEN"; return; }
  _A_TOKEN=$(kubectl -n "$NAMESPACE" get secret site-router-a-api-key \
    -o "jsonpath={.data.${API_KEY_FIELD}}" 2>/dev/null | base64 -d 2>/dev/null)
  echo "$_A_TOKEN"
}
_a_cilium_pod() {
  [ -n "${_A_CIL:-}" ] && { echo "$_A_CIL"; return; }
  node=$(kubectl -n "$NAMESPACE" get pod -l vm.kubevirt.io/name=site-router-a \
    -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)
  _A_CIL=$(kubectl -n cozy-cilium get pod -l k8s-app=cilium \
    --field-selector "spec.nodeName=$node" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  echo "$_A_CIL"
}

# ssh into B and run the command string "$1"
_ssh_b() {
  _exec sh -c \
    "sshpass -p '$REMOTE_B_PASS' ssh -o StrictHostKeyChecking=no \
      -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 admin@$(_b_ip) \"$1\"" 2>/dev/null
}

# Read a firewall counter of A, in packets, over the management API.
#   $1 = selector: "name TUNNEL-INGRESS" | "input filter" | "forward filter"
#   $2 = rule number, or "default" for the default action
# Only the curl runs in the pod. The `sh -c` string ends before the pipe, so the
# parsing runs in the shell of the script, which has jq. An unparseable answer
# yields no output rather than a 0, which is what lets guest_counter_flat tell a
# reading it could not take from one that did not move.
_a_counter() {
  path=$(printf '%s' "$1" | sed 's/ /","/g')
  _exec sh -c \
    "curl -k -s --max-time 90 https://$(_a_pod_ip)/show \
      --form-string 'data={\"op\":\"show\",\"path\":[\"firewall\",\"ipv4\",\"$path\"]}' \
      --form-string 'key=$(_a_token)'" 2>/dev/null |
    jq -r --arg rule "$2" '
      # `show firewall` prints one row per rule (Rule Action Protocol Packets
      # Bytes [Conditions]), so the packet count is the fourth field. The first
      # match wins; a missing row and a short row read as 0.
      [ (.data // "")
        | split("\n")[]
        | [ splits("[[:space:]]+") ]
        | map(select(. != ""))
        | select(length > 0 and .[0] == $rule)
        | (.[3] // "0")
      ] | (.[0] // "0")
    ' 2>/dev/null
}

# probe_from_b <proto> <dst> <port>
#   One probe from B, sourced from $PROBE_SRC, into the tunnel toward A.
#   Returns 0 when the destination was reached.
probe_from_b() {
  proto=$1; dst=$2; port=${3:-}
  case "$proto" in
    tcp)  _ssh_b "nc -z -w3 -s $PROBE_SRC $dst $port >/dev/null 2>&1 && echo R" | grep -q R ;;
    udp)  _ssh_b "echo x | nc -u -w2 -s $PROBE_SRC $dst $port >/dev/null 2>&1 && echo R" | grep -q R ;;
    icmp) _ssh_b "ping -c2 -W2 -I $PROBE_SRC $dst >/dev/null 2>&1 && echo R" | grep -q R ;;
    *) echo "probe_from_b: unknown proto $proto" >&2; return 2 ;;
  esac
}

# probe_from_b_stdout <proto> <dst> <port> <path>
#   An HTTP GET from B sourced from $PROBE_SRC. Prints the body: the check of the
#   source reads it from /clientip of the backend.
probe_from_b_stdout() {
  dst=$2; port=$3; path=${4:-/}
  _ssh_b "curl -s --max-time 8 --interface $PROBE_SRC http://$dst:$port$path"
}

# transfer_from_b <dst> <port> <megabytes>
#   Posts <megabytes> MB from B across the tunnel to /upload of agnhost netexec
#   and requires that the whole body was accepted. Without the clamp of the TCP
#   MSS the decrypted segments black-hole and the post hangs until the step
#   times out. Returns 0 on a transfer that completed.
transfer_from_b() {
  dst=$1; port=$2; mb=${3:-8}; bytes=$((mb*1024*1024))
  sent=$(_ssh_b "head -c $bytes /dev/zero | curl -s --max-time 60 --interface $PROBE_SRC -o /dev/null -w '%{size_upload}' -X POST --data-binary @- 'http://$dst:$port/upload'")
  sent=${sent%%.*}
  [ "${sent:-0}" -ge "$bytes" ] 2>/dev/null
}

# record <label> <value...>
#
# Writes an observation where the report is collected, and prints it. A
# Chainsaw script that fails reports only that it failed, so what the script saw,
# which is the whole content of the failure, would be lost without this.
record() {
  _rlabel="$1"; shift
  _rdir="${E2E_REPORT_DIR:-/tmp/site-router-e2e}/${TEST_NAME:-site-router}"
  mkdir -p "$_rdir" 2>/dev/null || true
  printf '%s: %s\n' "$_rlabel" "$*" >> "$_rdir/probe-observations.txt" 2>/dev/null || true
  echo "$_rlabel: $*"
}

# guest_counter_incremented <ruleset> <rule>
#   A drop counter of A moved since the snapshot of assert_dropped.
#   Rulesets: TUNNEL-INGRESS | input | forward.
guest_counter_incremented() {
  case "$1" in
    TUNNEL-INGRESS) sel="name TUNNEL-INGRESS" ;;
    input)          sel="input filter" ;;
    forward)        sel="forward filter" ;;
    *) echo "guest_counter_incremented: unknown ruleset $1" >&2; return 2 ;;
  esac
  # The step names the default row "default-action"; the appliance prints it as
  # "default".
  rule=$2; case "$rule" in default-action) rule=default ;; esac
  before=$(cat "$PROBE_STATE_DIR/ctr_${1}_${rule}" 2>/dev/null || echo 0)
  after=$(_a_counter "$sel" "$rule")
  case "${after:-}" in ''|*[!0-9]*) echo "guest_counter_incremented: could not read counter $1/$rule" >&2; return 1 ;; esac
  [ "${after:-0}" -gt "${before:-0}" ] 2>/dev/null
}

# guest_counter_flat <ruleset> <rule>
#   A drop counter of A did not move since the snapshot: positive evidence that
#   the probe was dropped before it reached this guard. Fails when the counter
#   cannot be read, so that "flat" is always backed by a reading.
guest_counter_flat() {
  case "$1" in
    TUNNEL-INGRESS) sel="name TUNNEL-INGRESS" ;;
    input)          sel="input filter" ;;
    forward)        sel="forward filter" ;;
    *) echo "guest_counter_flat: unknown ruleset $1" >&2; return 2 ;;
  esac
  rule=$2; case "$rule" in default-action) rule=default ;; esac
  before=$(cat "$PROBE_STATE_DIR/ctr_${1}_${rule}" 2>/dev/null || echo 0)
  after=$(_a_counter "$sel" "$rule")
  case "${after:-}" in ''|*[!0-9]*) echo "guest_counter_flat: could not read counter $1/$rule" >&2; return 1 ;; esac
  [ "${after:-0}" -le "${before:-0}" ] 2>/dev/null
}

# _snapshot_guards: what assert_dropped does before a probe. It reads each
# counter of interest.
_snapshot_guards() {
  for pair in "TUNNEL-INGRESS default" "input 1" "forward default"; do
    # shellcheck disable=SC2086
    set -- $pair
    case "$1" in
      TUNNEL-INGRESS) sel="name TUNNEL-INGRESS" ;;
      input) sel="input filter" ;;
      forward) sel="forward filter" ;;
    esac
    _a_counter "$sel" "$2" > "$PROBE_STATE_DIR/ctr_${1}_${2}" 2>/dev/null
  done
}

# assert_dropped <desc> <guard-check> <proto> <dst> [port]
#   The primitive of the negative matrix: a probe from B to <dst> must not
#   arrive, and the named guard must be the one that dropped it. Not arriving is
#   not enough, because it would also be true of a packet nothing ever sent.
assert_dropped() {
  desc=$1; guard=$2; proto=$3; dst=$4; port=${5:-}
  _snapshot_guards
  if probe_from_b "$proto" "$dst" "$port"; then
    echo "FAIL [$desc]: B reached $dst:$port, expected a drop" >&2
    return 1
  fi
  sleep 3   # let the counters settle
  # eval and not `sh -c`: the guard is a function of this file, which a child
  # shell cannot see.
  if ! eval "$guard"; then
    echo "FAIL [$desc]: $dst was not reached but the drop is not attributed to the guard ($guard)" >&2
    return 1
  fi
  echo "OK [$desc]: $dst dropped and attributed to its guard"
}
