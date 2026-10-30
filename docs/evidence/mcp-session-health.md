# MCP session and container health verification

Local verification on 2026-10-30 UTC, Docker Engine 29.8.1, Linux amd64.
All identifiers/stores are synthetic; no paired phone or production archive was used.

## Sessions and SDK contract

The MCP SDK is 2.3.0. Configuration defaults to 86400 seconds; zero disables
idle expiry and values above 315360000 seconds are refused before admin/app
listeners or the listening log. SSE/stdio do not receive the setting.

At the default 10000-session cap, new admission terminates and discards the
least recently active idle session. Existing requests retain SDK authentication,
credential-owner checks, DELETE behavior and JSON-RPC errors. Opening requests,
in-flight POSTs and open GET streams are protected, including when expiry is
disabled; when every session is active, new admission still returns 503.

`uv run pytest -q tests/test_session_idle.py tests/test_http_auth.py
 tests/test_http_oauth.py tests/test_oauth_dispatch_scopes.py
 tests/test_env_docs.py tests/test_agents_file_tree.py --tb=short`
passed 152 tests. The session tests include:

- Tiny caps with timeout 86400 and None, recency updates, actual transport
  termination, 404 JSON-RPC for the evicted ID, surviving sessions and fresh
  initialize/ping, with one eviction INFO containing only an eight-character ID prefix.
- Actual concurrent tool requests: a busy session survives while an idle
  neighbor is evicted, and a fully active cap returns JSON-RPC 503.
- Real SDK expiration at 0.2 seconds, a wait beyond that deadline, 404 using
  the actual issued session ID, then successful reinitialize and ping.
- SDK private attributes/methods and the default cap, plus behavior that fails
  if SDK changes prevent the admission adapter from operating.
- Invalid/huge settings exercised through the real Python entry point, with
  sentinels proving admin/app startup is never reached and no listening log.

Before the corrections the directed suite exited 1 with eight failures; after
correction it passed. No global SDK factory is patched. The app/lifespan remain
SDK-owned; only stateful admission and per-transport activity are wrapped.

## TCP and Unix socket probe

`uv run pytest -q tests/test_image_healthcheck.py --tb=short` passed 18 cases;
the Unix filesystem socket case is Linux-only and skipped on Windows. The
Dockerfile command itself runs against live/dead IPv4 and IPv6 listeners,
including unset/wildcard/trimmed hosts and accepted decimal port spellings.
No request reaches either the HTTP backend or inherited proxy. The changed
normalization cases first exited 1 with nine failures, then passed.

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
docker run -d --name lane-a-mcp-health-socket --network none \
  --tmpfs /app/store:rw,uid=1000,gid=1000 \
  --tmpfs /app/sockets:rw,mode=0700,uid=1000,gid=1000 \
  --health-interval=2s --health-start-interval=1s \
  --health-start-period=5s --health-retries=1 \
  -e WHATSAPP_MCP_TOKEN=test-session-token-0123456789 \
  -e WHATSAPP_MCP_SOCKET=/app/sockets/mcp.sock \
  whatsapp-mcp-server:lane-a-session-health
docker inspect --format '{{json .State.Health}}' lane-a-mcp-health-default
docker inspect --format '{{json .State.Health}}' lane-a-mcp-health-ipv6
docker inspect --format '{{json .State.Health}}' lane-a-mcp-health-socket
docker exec lane-a-mcp-health-socket python -c "import os; os.unlink('/app/sockets/mcp.sock')"
docker inspect --format '{{json .State.Health}}' lane-a-mcp-health-socket
docker exec lane-a-mcp-health-socket bash -c 'exec 3<>/dev/tcp/127.0.0.1/8000'
```

Built image: `sha256:47139ff5e7ea58bbdb0b1d8fb73fc839f600686bfe3ab02280d5c44ebc648eb4`.
The IPv4 container started at `03:36:09.203596974Z` and passed health at
`03:36:14.331235568Z`; IPv6 started at `03:36:11.334438821Z` and passed at
`03:36:16.455263Z` (about 5.1 s each). Both passed their next steady checks at
`03:38:14.349372102Z` / `03:38:16.473771532Z`, exit 0 with empty output.

The socket container was healthy before unlink (successful probe at
`03:36:42.780783885Z`) and became unhealthy after unlink (exit 1, empty output),
while an explicit TCP connect still exited 0. Only that disposable failure
probe used accelerated intervals/retries; the two default containers retained
the Dockerfile's 120 s interval, 15 s start period and 5 s startup interval.
All three logs had zero 401 lines. Bash and IPv6 `/dev/tcp/::1` were also
verified in the initial image. Temporary containers and the revised image were
removed after inspection:

```sh
docker rm -f lane-a-mcp-health-default lane-a-mcp-health-ipv6 lane-a-mcp-health-socket
docker image rm whatsapp-mcp-server:lane-a-session-health
```

The probe checks TCP and configured socket existence, not event-loop
responsiveness: a wedged loop with an open port/socket can remain healthy.
One Bash process still starts per probe (720/day at steady state instead of
2880 Python starts/day). This reduces process/import/slab pressure; **zero slab
growth and the issue #754 24-hour acceptance criterion have not been proved**.
Authenticated MCP/Unix responses remain separate smoke responsibilities.

## Gates and boundaries

Ruff format/check and Pyright passed on the reviewed code. The post-review full Windows run returned 3800 passed, 78 failed, 10 errors
and 65 skipped (exit 1 in 1340.81 s). Of these, 77 failures plus 10 errors
were the shell harness/native-path issue #762; the new failure was the metrics
test fake lacking an SDK session manager. That fake was replaced by the real
MCPServer with the same assertions, and rechecked in the final directed gate.
The transcription timing case passed in this full run. No post-fix full Windows
green result is claimed; complete Linux validation is the PR CI.
The initial full Windows run returned 3783 passed, 78 failed, 10 errors and
64 skipped (exit 1); the shell selection failures are tracked in
[#762](https://github.com/Tauri-EPO/whatsapp-mcp/issues/762), and the isolated-pass
transcription deadline case in [#763](https://github.com/Tauri-EPO/whatsapp-mcp/issues/763).
A failing-case native-shell recheck passed 15 merge-script cases, then stopped
on the proxy-smoke harness timeout; it is not complete Windows shell coverage.

The earlier head's Go race job failed in the unchanged large-contact operator
search test (503/count=0 on 20000 contacts), tracked in
[#769](https://github.com/Tauri-EPO/whatsapp-mcp/issues/769). No Go code changed.

Self-review used the local requesting-code-review criteria personally, following
the explicit WORK ALONE instruction. Paired-phone and real remote-client behavior,
24-hour memory behavior and eventual merge/deploy are outside this verification.
CI results are attached to the precise published PR head; no merge was performed.

Final directed gate after that fixture correction: **177 passed, 1 skipped**
(exit 0 in 140.36 s), covering observability, sessions, auth/OAuth, env/tree docs
and the Dockerfile probe. Ruff format left all 158 files unchanged, Ruff check
passed, and Pyright reported zero errors/warnings.
