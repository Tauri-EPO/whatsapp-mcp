# MCP HTTP session and TCP healthcheck verification

Local verification on 2026-10-30 UTC, Docker Engine 29.8.1, Linux amd64.
No paired phone, production store or external network access was used by the test containers.

## HTTP sessions

`uv run pytest -q tests/test_session_idle.py tests/test_http_auth.py tests/test_env_docs.py`
passed 63 tests. The new tests exercise the installed MCP SDK 2.3.0 through
`build_http_app`, including the actual session manager value (86400, None, 120),
invalid configuration refusal, SSE exclusion, and authenticated HTTP dispatch:
unknown session -> 404 with JSON-RPC error -> initialize -> successful ping,
all within one app lifespan. The test uses an unknown session ID, not a timed
expiry or a real remote MCP client.

## Container health

```sh
docker build -t whatsapp-mcp-server:lane-a-session-health ./whatsapp-mcp-server
docker run -d --name lane-a-mcp-health-default --network none \
  --tmpfs /app/store:rw,uid=1000,gid=1000 \
  -e WHATSAPP_MCP_TOKEN=test-session-token-0123456789 \
  whatsapp-mcp-server:lane-a-session-health
docker run -d --name lane-a-mcp-health-ipv6 --network none \
  --tmpfs /app/store:rw,uid=1000,gid=1000 \
  -e WHATSAPP_MCP_TOKEN=test-session-token-0123456789 \
  -e WHATSAPP_MCP_HOST=:: -e WHATSAPP_MCP_PORT=8123 \
  whatsapp-mcp-server:lane-a-session-health
docker inspect --format '{{json .State.Health}}' lane-a-mcp-health-default
docker inspect --format '{{json .State.Health}}' lane-a-mcp-health-ipv6
docker logs lane-a-mcp-health-default
docker logs lane-a-mcp-health-ipv6
docker exec lane-a-mcp-health-default bash --version
docker exec lane-a-mcp-health-ipv6 bash -c 'exec 3<>/dev/tcp/::1/8123'
docker exec lane-a-mcp-health-default bash -c 'exec 3<>/dev/tcp/127.0.0.1/1'
```

Built image: `sha256:d2a2665a18fdaa1e6d7da4c77715884d3ea2559f874def10ec188ada90e839b3`.
Both containers reported `Status=healthy`, `FailingStreak=0`, probe exit 0 and
empty probe output. The IPv4 container started at `02:07:22.541930336Z` and
passed its first healthcheck at `02:07:27.708917944Z`; IPv6 started at
`02:07:23.505200042Z` and passed at `02:07:28.617579898Z` (about 5.1 s each).
Both logs had zero 401 lines. Bash 5.2.37 was present; the explicit IPv6 TCP
connect exited 0 and the closed IPv4 port exited 1 with `Connection refused`.
The next steady-state checks also passed: IPv4 ended at `02:09:27.727305718Z`
and IPv6 at `02:09:28.644499045Z`, 120 seconds after their first checks.
Both logs still had zero 401 lines when inspected again before cleanup.
Running the image with `WHATSAPP_MCP_SESSION_IDLE_TIMEOUT_S=-1` refused startup
with `Invalid WHATSAPP_MCP_SESSION_IDLE_TIMEOUT_S; must be a nonnegative integer`
and exit 1. The temporary containers and image were removed successfully.

`uv run pytest -q tests/test_image_healthcheck.py` passed all 12 cases,
executing the Dockerfile's command against live/closed IPv4 and IPv6 listeners
with inherited proxy settings. No HTTP request reached the backend or proxy.
No Compose override defines an MCP healthcheck. `probe_http_health` is retained
for the separate socket smoke/tests. CI's Compose smoke waits 90 seconds, above
the observed startup time; CI verifies the complete unpaired Compose topology.

Cleanup after inspection:

```sh
docker rm -f lane-a-mcp-health-default lane-a-mcp-health-ipv6
docker image rm whatsapp-mcp-server:lane-a-session-health
```

This verifies startup and probe behavior. It does not establish 24-hour cgroup
slab behavior; no 24-hour memory soak was run.

## Local gates and review

Ruff format left all 157 files unchanged, Ruff check passed, and Pyright
reported zero errors/warnings. The full Windows pytest run was executed once:
`78 failed, 3783 passed, 64 skipped, 1 warning, 10 errors in 1142.44s` (exit 1).
The new session/healthcheck tests passed in that run. Failures were outside
the changed files:

- 77 failures plus 10 fixture errors came from shell tests choosing the
  Windows System32 WSL Bash shim and handing it native Windows paths. The
  scripts were not reached (missing-path errors/exit 127). Tracked in
  [#762](https://github.com/Tauri-EPO/whatsapp-mcp/issues/762).
- `test_whole_file_budget_includes_conversion_and_all_parts` raised the
  expected deadline error but exceeded its 4.8 s wall bound (7.287 s). Its
  isolated rerun passed (`1 passed in 3.87s`). Tracked in
  [#763](https://github.com/Tauri-EPO/whatsapp-mcp/issues/763), related to #688.

Self-review checked the complete diff against both issue requirements, SDK
wiring, auth refusal coverage, bind normalization, Docker startup/steady
health checks, documentation and test assertions. No blocking code findings.
The local `requesting-code-review` criteria were applied personally, following
the dispatch's explicit instruction to work alone. Review excludes a 24-hour
slab soak and paired-phone behavior; both are unverified. The complete Linux
CI/Compose results are reported in the pull request checks.
