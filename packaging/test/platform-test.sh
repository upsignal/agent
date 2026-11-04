#!/usr/bin/env bash
# Installs the .deb into a container running real systemd and checks the package and the hardened service.
# Needs Docker and no Upsignal API: the agent runs against an unreachable URL and spools what it collects.
#
#   packaging/test/platform-test.sh <base-image> <package.deb> <newer-package.deb>
#   e.g. packaging/test/platform-test.sh ubuntu:24.04 dist/upsignal-agent_1.0.0_amd64.deb /tmp/upgrade.deb
set -euo pipefail

BASE=$1
DEB=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
UPGRADE=$(cd "$(dirname "$3")" && pwd)/$(basename "$3")
IMAGE=upsignal-agent-platform:$(printf '%s' "$BASE" | tr ':/.' '---')
C=upsignal-agent-platform-$$
STATE=/var/lib/upsignal-agent

failures=0
sut() { docker exec -i "$C" "$@"; }
pass() { echo "ok: $1"; }
fail() {
	echo "FAIL: $1"
	failures=$((failures + 1))
}
check() {
	local what=$1
	shift
	if "$@" >/dev/null 2>&1; then pass "$what"; else fail "$what"; fi
}
wait_for() {
	local n=$1
	shift
	for _ in $(seq "$n"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done
	return 1
}
main_pid() { sut systemctl show -p MainPID --value upsignal-agent; }
# True once the main process has exec'd the agent. Before that it is systemd's forked child: still root and
# still holding capabilities while it sets up the sandbox. Checked by process name: until exec, systemd names the child "(upsignal-agent)" (truncated), and /proc/PID/exe
# is unreliable under CPU emulation.
agent_running() { [ "$(sut cat "/proc/$(main_pid)/comm")" = upsignal-agent ]; }
is_active() { [ "$(sut systemctl is-active upsignal-agent)" = active ]; }
cleanup() {
	if [ "$failures" -gt 0 ]; then sut journalctl -u upsignal-agent --no-pager -n 40 || true; fi
	docker rm -f "$C" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "=== $BASE: $(basename "$DEB")"
docker build -q -t "$IMAGE" --build-arg BASE="$BASE" - >/dev/null <<'EOF'
ARG BASE
FROM ${BASE}
RUN apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends \
      systemd systemd-sysv ca-certificates iproute2 procps util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl mask getty.target console-getty.service systemd-logind.service >/dev/null 2>&1 || true
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
EOF
docker run -d --name "$C" --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
	-v "$DEB:/pkg/current.deb:ro" -v "$UPGRADE:/pkg/upgrade.deb:ro" "$IMAGE" >/dev/null
wait_for 60 bash -c "docker exec $C systemctl is-system-running 2>/dev/null | grep -Eq 'running|degraded'" ||
	{ fail "systemd did not boot"; exit 1; }

echo "--- install"
sut sh -c 'DEBIAN_FRONTEND=noninteractive apt-get install -y -q /pkg/current.deb >/dev/null 2>&1'
check "package installed" sut dpkg -s upsignal-agent
check "system user without a login shell" bash -c "docker exec $C getent passwd upsignal-agent | grep -q ':/nonexistent:/usr/sbin/nologin$'"
check "binary 0755 root" bash -c "[ \"\$(docker exec $C stat -c '%a %U' /usr/bin/upsignal-agent)\" = '755 root' ]"
check "config 0640 root:upsignal-agent" bash -c "[ \"\$(docker exec $C stat -c '%a %U:%G' /etc/upsignal-agent/config.yaml)\" = '640 root:upsignal-agent' ]"
check "state dir 0700 upsignal-agent" bash -c "[ \"\$(docker exec $C stat -c '%a %U' $STATE)\" = '700 upsignal-agent' ]"
sut systemctl start upsignal-agent || true
check "inactive until enrolled" bash -c "! docker exec $C systemctl is-active upsignal-agent"

echo "--- run (simulated enrollment against an unreachable API)"
# The credential's base URL must match the config, so point both at a reserved, unresolvable name.
sut sed -i 's|^  base_url:.*|  base_url: "https://upsignal.invalid"|' /etc/upsignal-agent/config.yaml
sut sh -c "printf '%s' '{\"agentId\":\"test\",\"monitorId\":\"test\",\"credential\":\"not-a-real-secret\",\"baseUrl\":\"https://upsignal.invalid\",\"enrolledAt\":\"2026-01-01T00:00:00Z\"}' >$STATE/agent-credential && chown upsignal-agent: $STATE/agent-credential && chmod 600 $STATE/agent-credential"
sut systemctl enable --now upsignal-agent >/dev/null 2>&1
check "active once enrolled" wait_for 15 is_active
check "agent binary running" wait_for 15 agent_running
PID=$(main_pid)
check "runs as upsignal-agent" bash -c "[ \"\$(docker exec $C ps -o user:32= -p $PID)\" = upsignal-agent ]"
check "no effective capabilities" bash -c "docker exec $C grep -Eq '^CapEff:\s+0+$' /proc/$PID/status"
check "no listening sockets" bash -c "! docker exec $C ss -lntup | grep -q upsignal"
AS_AGENT_ARGS=(nsenter -t "$PID" -m setpriv --reuid upsignal-agent --regid upsignal-agent --clear-groups)
AS_AGENT="${AS_AGENT_ARGS[*]}"
check "sandbox: /etc read-only" bash -c "! docker exec $C $AS_AGENT touch /etc/upsignal-agent/probe"
check "sandbox: /usr read-only" bash -c "! docker exec $C $AS_AGENT touch /usr/bin/probe"
check "sandbox: state dir writable" docker exec "$C" "${AS_AGENT_ARGS[@]}" sh -c "touch $STATE/.probe && rm $STATE/.probe"
exposure=$(sut systemd-analyze security upsignal-agent --no-pager 2>/dev/null | sed -nE 's/.*exposure level for upsignal-agent.service: ([0-9.]+).*/\1/p')
check "systemd-analyze exposure ${exposure:-?} <= 2.0" awk -v e="${exposure:-99}" 'BEGIN { exit !(e <= 2.0) }'
check "collected samples spooled while the API is unreachable" \
	wait_for 45 bash -c "docker exec $C sh -c 'ls $STATE/spool/*.json' | grep -q json"
check "no collector errors under the sandbox" bash -c "! docker exec $C journalctl -u upsignal-agent --no-pager | grep -q 'collector error'"
check "credential not in the journal" bash -c "! docker exec $C journalctl -u upsignal-agent --no-pager | grep -q not-a-real-secret"

echo "--- crash restart"
sut kill -SEGV "$PID"
check "restarted after a crash" wait_for 25 bash -c "[ \"\$(docker exec $C systemctl show -p MainPID --value upsignal-agent)\" != '$PID' ] && [ \"\$(docker exec $C systemctl is-active upsignal-agent)\" = active ]"

echo "--- upgrade"
sut sed -i 's/level: info/level: debug/' /etc/upsignal-agent/config.yaml
CRED=$(sut sha256sum $STATE/agent-credential)
SEQ=$(sut cat $STATE/sequence)
PID=$(main_pid)
sut sh -c 'DEBIAN_FRONTEND=noninteractive apt-get install -y -q /pkg/upgrade.deb >/dev/null 2>&1'
WANT=$(sut dpkg-deb -f /pkg/upgrade.deb Version)
check "upgraded to $WANT" bash -c "[ \"\$(docker exec $C dpkg-query -W -f '\${Version}' upsignal-agent)\" = '$WANT' ]"
check "service restarted on upgrade" wait_for 20 bash -c "[ \"\$(docker exec $C systemctl show -p MainPID --value upsignal-agent)\" != '$PID' ] && [ \"\$(docker exec $C systemctl is-active upsignal-agent)\" = active ]"
check "credential kept" bash -c "[ \"\$(docker exec $C sha256sum $STATE/agent-credential)\" = '$CRED' ]"
check "sequence kept" bash -c "[ \"\$(docker exec $C cat $STATE/sequence)\" -ge '$SEQ' ]"
check "config edit kept" docker exec "$C" grep -q 'level: debug' /etc/upsignal-agent/config.yaml
check "config permissions kept" bash -c "[ \"\$(docker exec $C stat -c '%a %U:%G' /etc/upsignal-agent/config.yaml)\" = '640 root:upsignal-agent' ]"
check "spool kept" bash -c "docker exec $C sh -c 'ls $STATE/spool/*.json' | grep -q json"

echo "--- remove and purge"
sut sh -c 'apt-get remove -y -q upsignal-agent >/dev/null 2>&1'
check "remove stops the service and keeps the enrollment" bash -c "! docker exec $C systemctl is-active upsignal-agent && docker exec $C test -e $STATE/agent-credential"
sut sh -c 'apt-get purge -y -q upsignal-agent >/dev/null 2>&1'
check "purge deletes config and state" bash -c "docker exec $C sh -c '! [ -e /etc/upsignal-agent ] && ! [ -e $STATE ]'"

if [ "$failures" -gt 0 ]; then
	echo "=== $BASE: $failures check(s) failed"
	exit 1
fi
echo "=== $BASE: all checks passed"
