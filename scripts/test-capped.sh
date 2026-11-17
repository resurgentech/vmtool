#!/usr/bin/env bash
# test-capped.sh [go test args] — run `go test` (default ./...) inside a
# capped systemd --user scope, so a runaway test (for example one that
# re-executes its own binary, as on 2026-11-17) is stopped by the kernel
# instead of taking the machine down. `just test`, `just check` and the orch
# commit hook all come through here.
#
# The scope's pids.max holds everything it starts, including processes that
# outlive `go test`. Without a working `systemd-run --user` the tests still
# run, uncapped, with a warning on stderr.
#
# VMTOOL_TEST_TASKS_MAX  scope TasksMax (threads count). Default TASKS_MAX below.
# VMTOOL_TEST_CPU_QUOTA  scope CPUQuota, e.g. 800%. Default online CPUs x 50%
#                        (half the cores).
set -euo pipefail

# TasksMax default: about twice the largest pids.peak of a normal full run.
# Measured 2026-11-17 on jetfire (16 cores, Go 1.27, warm build cache),
# `go test -count=1 ./...` in a TasksMax=1024 scope, three runs: pids.peak
# 114, 112, 114. A forced full rebuild (`go test -a -count=1 ./...`) also
# peaked at 114. 2 x 114 = 228, rounded up to 256. Threads per Go process grow
# with the core count, so a much bigger machine may need
# VMTOOL_TEST_TASKS_MAX raised.
TASKS_MAX=256

if [ $# -eq 0 ]; then
	set -- ./...
fi

if ! command -v systemd-run >/dev/null 2>&1 || ! systemd-run --user --scope -q true >/dev/null 2>&1; then
	echo "test-capped: systemd-run --user not available; running go test without a resource cap" >&2
	exec go test "$@"
fi

# From here on any error ends the script: it never falls back to uncapped.
# The CPU count is getconf's online CPUs, not nproc: inside a scope (say this
# script run under another systemd-run) nproc reports the cgroup's quota.
cpus=$(getconf _NPROCESSORS_ONLN)
tasks_max=${VMTOOL_TEST_TASKS_MAX:-$TASKS_MAX}
cpu_quota=${VMTOOL_TEST_CPU_QUOTA:-$((cpus * 50))%}
# An explicit unit name: systemd-run's default is derived from the PID, which
# an exec chain (systemd-run -> this script -> systemd-run) keeps, so the
# default would collide with the outer scope.
exec systemd-run --user --scope -q --unit="vmtool-test-$$-$RANDOM" \
	-p "TasksMax=$tasks_max" -p "CPUQuota=$cpu_quota" -- go test "$@"
