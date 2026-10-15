#!/usr/bin/env bash
# Installs the real bilgie agent on Linux, waits for the scheduler to fire one
# tick, then uninstalls it. It changes the invoking user's scheduler and state
# dir, so it refuses to run outside a throwaway session.
#
# usage: BILGIE_OS_E2E=1 agent-linux.sh <bilgie-binary> <systemd|cron>
set -euo pipefail

bin=${1:?usage: agent-linux.sh <bilgie-binary> <systemd|cron>}
backend=${2:?usage: agent-linux.sh <bilgie-binary> <systemd|cron>}

if [ "${BILGIE_OS_E2E:-}" != 1 ]; then
  echo "refusing to touch the user's scheduler; set BILGIE_OS_E2E=1 in a throwaway session" >&2
  exit 2
fi
bin=$(readlink -f "$bin")

state_dir=$HOME/.local/state/bilgie
tick_file=$state_dir/tick.json
unit_dir=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user
wait_seconds=${BILGIE_TICK_WAIT:-180}

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

step() {
  echo "::group::$*"
}

wait_for_tick() {
  local waited=0
  until [ -s "$tick_file" ]; do
    if [ "$waited" -ge "$wait_seconds" ]; then
      fail "no tick within ${wait_seconds}s"
    fi
    sleep 5
    waited=$((waited + 5))
  done
  echo "tick fired after ~${waited}s"
  cat "$tick_file"
  echo
  jq -e '.time and .tier and .action' "$tick_file" >/dev/null || fail "tick.json lacks time, tier or action"
}

systemd_run() {
  systemctl --user show-environment >/dev/null || fail "no systemd user manager answers"
  step "install (systemd)"
  "$bin" install-agent
  echo "::endgroup::"

  step "verify installed units"
  [ -f "$unit_dir/bilgie.service" ] || fail "missing $unit_dir/bilgie.service"
  [ -f "$unit_dir/bilgie.timer" ] || fail "missing $unit_dir/bilgie.timer"
  systemctl --user is-enabled bilgie.timer
  systemctl --user is-active bilgie.timer
  systemctl --user list-timers bilgie.timer --no-pager
  systemd-analyze --user verify "$unit_dir/bilgie.service" "$unit_dir/bilgie.timer"
  [ ! -e "$tick_file" ] || fail "tick.json exists before the timer fired"
  echo "::endgroup::"

  step "wait for the timer to run one tick"
  wait_for_tick
  # tick.json is written before the pass runs, so wait for the unit to
  # finish before trusting Result and ExecMainStatus.
  local settle=0
  while [ "$(systemctl --user show bilgie.service -p ActiveState --value)" = activating ] ||
    [ "$(systemctl --user show bilgie.service -p ActiveState --value)" = active ]; do
    [ "$settle" -lt 120 ] || fail "service still running after ${settle}s"
    sleep 1
    settle=$((settle + 1))
  done
  systemctl --user show bilgie.service -p Result -p ExecMainStatus -p ActiveState
  [ "$(systemctl --user show bilgie.service -p Result --value)" = success ] || fail "service run did not succeed"
  [ "$(systemctl --user show bilgie.service -p ExecMainStatus --value)" = 0 ] || fail "tick exited non-zero"
  echo "::endgroup::"

  step "uninstall (systemd)"
  "$bin" uninstall-agent
  [ ! -e "$unit_dir/bilgie.service" ] || fail "service unit left behind"
  [ ! -e "$unit_dir/bilgie.timer" ] || fail "timer unit left behind"
  [ ! -e "$tick_file" ] || fail "tick.json left behind"
  if systemctl --user is-enabled bilgie.timer >/dev/null 2>&1; then
    fail "timer is still enabled"
  fi
  [ -z "$(systemctl --user list-timers --all --no-legend bilgie.timer)" ] || fail "timer still listed"
  echo "::endgroup::"
}

cron_run() {
  command -v crontab >/dev/null || fail "crontab is not installed"
  # A bogus runtime dir makes `systemctl --user` fail, which is the headless
  # case that selects the cron fallback.
  local bogus=/nonexistent-runtime-dir
  step "install (cron fallback)"
  XDG_RUNTIME_DIR=$bogus DBUS_SESSION_BUS_ADDRESS='' "$bin" install-agent
  crontab -l
  crontab -l | grep -q ' # bilgie$' || fail "no tagged crontab entry"
  [ ! -e "$unit_dir/bilgie.timer" ] || fail "cron install left a timer unit"
  [ ! -e "$tick_file" ] || fail "tick.json exists before cron fired"
  echo "::endgroup::"

  step "wait for cron to run one tick"
  wait_for_tick
  # cron reports no exit status, so wait for the tick to end and require a
  # clean log instead.
  local settle=0
  while pgrep -f "$bin tick" >/dev/null; do
    [ "$settle" -lt 120 ] || fail "tick still running after ${settle}s"
    sleep 1
    settle=$((settle + 1))
  done
  if grep -qiE '^error|panic' "$state_dir/auto.log"; then
    cat "$state_dir/auto.log"
    fail "tick logged an error"
  fi
  echo "::endgroup::"

  step "uninstall (cron fallback)"
  XDG_RUNTIME_DIR=$bogus DBUS_SESSION_BUS_ADDRESS='' "$bin" uninstall-agent
  if crontab -l 2>/dev/null | grep -q ' # bilgie$'; then
    fail "crontab entry left behind"
  fi
  [ ! -e "$tick_file" ] || fail "tick.json left behind"
  echo "::endgroup::"
}

rm -rf "$state_dir"
case "$backend" in
  systemd) systemd_run ;;
  cron) cron_run ;;
  *) fail "unknown backend $backend" ;;
esac
echo "OK: $backend install, tick and uninstall"
