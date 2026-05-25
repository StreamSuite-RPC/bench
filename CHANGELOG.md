# Changelog

## v1.0.4 — 2026-05-25
- Unmistakable SLA-scope banner at top of every run (RPC→BSC, not user→BSC)
- Output restructured into 3 clear sections: YOUR network / OUR server / TOTAL
- Default-on public-RPC comparison (Binance dataseed, PublicNode, LlamaRPC)
- Interpretation footer with actionable hints (corporate-proxy detection, distance-from-Ashburn advice, refund-eligibility guidance on misses)
- New `--no-compare` flag to opt out of the public-RPC comparison

## v1.0.2 — 2026-05-25
- ci: add cross-platform smoke test (matrix runs the actual binary on Linux/macOS/Windows after each release)

## v1.0.1 — 2026-05-25
- fix: windows binary no longer named `.exe.exe`
- add: `.tar.gz` (unix) and `.zip` (windows) archive variants alongside raw binaries

## v1.0.0 — 2026-05-25
- initial release
