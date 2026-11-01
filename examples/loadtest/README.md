# Live-page load test

A repeatable load test of live pages: a dashboard over Postgres (`main.vuka`,
`pages/`, `components/`, `store/`, 25 instances, one refresh running five
queries) and a Go client (`client/`) behaving as the browser's runtime does. It
GETs the page for its `data-vk-token` and the refresh button's handler id, dials
`/_vuka/live?t=`, joins with `{"type":"join","ref":1,"v":2}`, waits for `render`,
then sends one `{"type":"event","target":"cN:M","event":"click","ref":N}` per
period and matches replies by ref. The server lifts the caps with
`web.LiveMaxConns(0)` and `web.LiveMaxConnsPerIP(0)`.

## Running it

It's a module of its own, so the framework's module doesn't need the database
driver. From this directory, with Postgres running:

```sh
createdb loadtest
export LOADTEST_DB='postgres://you@127.0.0.1:5432/loadtest?sslmode=disable&pool_max=20&pool_max_idle=20'
mkdir -p build
vuka build -o build/server .
go build -o build/client ./client
./run.sh 1000 out/1000            # latencies
./run.sh 10000 out/10000 profile  # also CPU, heap, goroutine, block and mutex profiles
dropdb loadtest
```

`run.sh` waits for the machine's load to fall below `MAX_LOAD` (up to ten
minutes), starts the server with `GODEBUG=gctrace=1`, runs the client, and stops
the server if it passes `MAX_RSS_KB` (7 GB) or swap grows by more than
`MAX_SWAP_GROWTH_MB`. Its environment is at the top of the script.

Each run's directory holds:

| File | What |
|---|---|
| `result.json` | latencies, the client's own lag, slow replies per second, the server's stats during the window |
| `conditions.txt` | load and swap before and after |
| `samples.txt` | RSS, swap and load every 5 s |
| `server.log` | the gctrace |
| `*.pprof`, `goroutine.txt` | with `profile`, taken 10 s into the window |

## Reading the numbers

Say what the machine was doing with every number. A client, server and database
on one machine compete for it. Stay well under the ephemeral port range
(16,384 ports on macOS) and watch for swapping. `client.lag_ms` in `result.json`
is the client's own scheduling delay, which would look like a slow reply: check
it's small before blaming the server. Block and mutex profiling cost something,
so compare latencies only between runs without `profile`.
