# streamsuite-bench

Measure your real RPC latency to [StreamSuite](https://streamsuite.io) from where your bot lives. Reports two numbers separately — your network distance, and our server-side processing — so you know what we can SLA and what we can't.

## Install

```sh
curl -sSL https://streamsuite.io/bench/install.sh | sh
```

Inspect the script first if you'd like:

```sh
curl -sSL https://streamsuite.io/bench/install.sh
```

Manual download: [latest release](https://github.com/StreamSuite-RPC/bench/releases/latest). Static binary, ~5 MB, no dependencies, MIT-licensed.

## Use

```sh
# default: 1000 × eth_blockNumber against va-bsc-01.streamsuite.io
./streamsuite-bench

# compare against your current RPC
./streamsuite-bench --vs https://your-current-rpc.example/<YOUR_KEY>

# heavier run, JSON output for CI
./streamsuite-bench --n 5000 --c 4 --json
```

Sample output:

```
  streamsuite-bench v1.0.0
  detected location:  aws/us-east-1
  1000 × eth_blockNumber  →  https://va-bsc-01.streamsuite.io

  Network RTT  (TCP-SYN → :443):           1.1 ms
  Server proc  (RPC p99 − network):        0.6 ms
  ──────────────────────────────────────────────
  Total RPC RTT (p99):                     1.7 ms

  Server SLA target (p99 ≤ 5 ms): PASS (0.6 ms)
```

## What it measures

- **Network RTT** — TCP-SYN handshake to `va-bsc-01.streamsuite.io:443`. This is the physical-distance lower bound. Not in anyone's control but physics.
- **Server processing** — RPC RTT minus network RTT. This is what we SLA. Below the [tier target](https://streamsuite.io/pricing), or refund.
- **Total RPC RTT** — what you actually feel, end to end.

## What it doesn't measure

- DNS lookup (done once, cached)
- Your own bot's internal processing
- WebSocket subscription latency (separate workload; coming in a future version)

## Refund SLA

The `server_p99_ms` field of the JSON output is what the [refund policy](https://streamsuite.io/legal/refunds) is pinned to. Network RTT is your own infrastructure and is not refund-eligible.

## Build from source

```sh
git clone https://github.com/StreamSuite-RPC/bench
cd bench
go build -o streamsuite-bench
```

Requires Go 1.23+.

## License

MIT. See [LICENSE](LICENSE).
