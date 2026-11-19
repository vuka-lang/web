#!/bin/sh
# run.sh sockets out-dir [profile]: one run of the load test, server and client
# built already (see README.md). With "profile" it also takes CPU, heap,
# goroutine, block and mutex profiles while measuring, which costs a little:
# compare latencies between runs without it.
#
# Environment: SCENARIO (pg, or mem for no database), SERVER, CLIENT (the
# binaries), LOADTEST_DB, SERVER_PROCS (4), CLIENT_PROCS (3), EVERY (5s),
# MEASURE (120s), WARMUP (30s), RATE (200), PORTS (one per 8,000 sockets: the
# ephemeral ports of one address are about 16,000), STEP (1000), SETTLE (10s),
# PHASES (the client's default for the scenario), SERVER_ARGS, CLIENT_ARGS,
# MAX_RSS_KB (7 GB), MAX_SWAP_GROWTH_MB (1024), MAX_LOAD (6).
set -u
sockets=$1
out=$2
profile=${3:-}
: "${SCENARIO:=pg}" "${SERVER:=./build/server}" "${CLIENT:=./build/loadclient}"
: "${SERVER_PROCS:=4}" "${CLIENT_PROCS:=3}" "${EVERY:=5s}" "${MEASURE:=120s}" "${WARMUP:=30s}" "${RATE:=200}"
: "${PORTS:=$(((sockets + 7999) / 8000))}" "${STEP:=1000}" "${SETTLE:=10s}" "${PHASES:=}" "${CLIENT_ARGS:=}"
: "${MAX_RSS_KB:=7340032}" "${MAX_SWAP_GROWTH_MB:=1024}" "${MAX_LOAD:=6}" "${SERVER_ARGS:=}"
debug=127.0.0.1:18099
mkdir -p "$out"

# One socket is a file descriptor in the client and one in the server, each
# a process of its own that inherits this limit.
need=$((sockets + 512))
for lim in "$(ulimit -Hn)" "$(sysctl -n kern.maxfilesperproc 2>/dev/null)" "$need"; do
	ulimit -S -n "$lim" 2>/dev/null && break
done
have=$(ulimit -n)
if [ "$(uname)" = Darwin ]; then
	cap=$(sysctl -n kern.maxfilesperproc)
	if [ "$have" = unlimited ] || [ "$have" -gt "$cap" ]; then have=$cap; fi
fi
if [ "$have" != unlimited ] && [ "$have" -lt "$need" ]; then
	echo "open files: $sockets sockets need $need descriptors per process, the limit is $have: raise ulimit -n (and kern.maxfilesperproc on macOS)" >&2
	exit 1
fi

load1() { uptime | sed 's/.*load averages*: *//; s/,/ /g' | awk '{print $1}'; }
swap_mb() {
	if [ "$(uname)" = Darwin ]; then
		sysctl -n vm.swapusage | sed 's/.*used = \([0-9.]*\)M.*/\1/' | cut -d. -f1
	else
		awk '/SwapTotal/{t=$2}/SwapFree/{f=$2}END{print int((t-f)/1024)}' /proc/meminfo
	fi
}

# Other work on the machine skews a run: wait for it, up to ten minutes.
i=0
while [ "$i" -lt 120 ] && awk "BEGIN{exit !($(load1) >= $MAX_LOAD)}"; do
	i=$((i + 1))
	sleep 5
done
start_load=$(load1)
start_swap=$(swap_mb)
echo "load before: $start_load, swap used: ${start_swap} MB, waited $((i * 5))s, open files limit: $have, ports: $PORTS" | tee "$out/conditions.txt"

prof=""
[ -n "$profile" ] && prof="-block-rate 1000000 -mutex-fraction 100"
# shellcheck disable=SC2086
GOMAXPROCS=$SERVER_PROCS GODEBUG=gctrace=1 "$SERVER" -scenario "$SCENARIO" -ports "$PORTS" -debug "$debug" $prof $SERVER_ARGS >"$out/server.log" 2>&1 &
server=$!
stop() { kill "$server" 2>/dev/null; wait "$server" 2>/dev/null; kill "${sampler:-0}" 2>/dev/null; }
trap stop EXIT
i=0
until curl -fs "http://$debug/debug/stats" >/dev/null; do
	i=$((i + 1))
	if [ "$i" -gt 240 ]; then echo "the server didn't start"; exit 1; fi
	sleep 0.5
done

# Stops the run when the machine is in trouble.
(
	echo "seconds rss_kb swap_mb load"
	t=0
	while kill -0 "$server" 2>/dev/null; do
		rss=$(ps -o rss= -p "$server" | tr -d ' ')
		swap=$(swap_mb)
		echo "$t $rss $swap $(load1)"
		if [ "${rss:-0}" -gt "$MAX_RSS_KB" ] || [ $((swap - start_swap)) -gt "$MAX_SWAP_GROWTH_MB" ]; then
			echo "STOP rss=$rss swap=$swap" >&2
			kill "$server"
			pkill -P $$ -f client 2>/dev/null
			break
		fi
		sleep 5
		t=$((t + 5))
	done
) >"$out/samples.txt" 2>>"$out/conditions.txt" &
sampler=$!

mark=$out/.measuring
rm -f "$mark"
# shellcheck disable=SC2086
GOMAXPROCS=$CLIENT_PROCS "$CLIENT" -scenario "$SCENARIO" -ports "$PORTS" -sockets "$sockets" -rate "$RATE" -every "$EVERY" -warmup "$WARMUP" -measure "$MEASURE" -step "$STEP" -settle "$SETTLE" -phases "$PHASES" -debug "http://$debug" -mark "$mark" -out "$out/result.json" $CLIENT_ARGS >"$out/client.log" 2>&1 &
client=$!

if [ -n "$profile" ]; then
	while [ ! -e "$mark" ] && kill -0 "$client" 2>/dev/null; do sleep 1; done
	sleep 10
	curl -fs "http://$debug/debug/pprof/profile?seconds=30" -o "$out/cpu.pprof"
	curl -fs "http://$debug/debug/pprof/heap" -o "$out/heap.pprof"
	curl -fs "http://$debug/debug/pprof/heap?gc=1" -o "$out/heap_gc.pprof"
	curl -fs "http://$debug/debug/pprof/goroutine" -o "$out/goroutine.pprof"
	curl -fs "http://$debug/debug/pprof/goroutine?debug=1" -o "$out/goroutine.txt"
	curl -fs "http://$debug/debug/pprof/block" -o "$out/block.pprof"
	curl -fs "http://$debug/debug/pprof/mutex" -o "$out/mutex.pprof"
fi
wait "$client"
code=$?
curl -fs "http://$debug/debug/stats" -o "$out/stats_end.json"
ps -o rss= -p "$server" >"$out/rss_end_kb.txt"
echo "load after: $(load1), swap used: $(swap_mb) MB" | tee -a "$out/conditions.txt"
tail -1 "$out/client.log"
exit $code
