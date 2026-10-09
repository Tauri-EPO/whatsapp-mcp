# #642: operator and MCP admin two-stack proof

Observed on 2026-10-09 with Docker Engine 29.8.1 / Compose 5.5.1,
Windows Docker Desktop Linux engine. The runtime sources match PR #675's
locally verified Go/Python sources, including rejection of zero-frame audio.
The run was repeated after that audio fix. Bridge image:
`sha256:a28a3d888d200d5a02e5f240a18f1db4dfb6cb1756fc03d98dc14fd3adf52484`;
MCP image:
`sha256:c85ba117aad55f8de51aaab8b1043983ce6b10cfeb7e5715a5babf90c62d759e`.

The disposable `scripts/smoke-proxy.sh` run used cached local images and a
sanitized environment, first proxy-only and then proxy+operator, with two
projects running together in each mode. Five Compose configurations passed.
The combined run also executed an independent container on the private
operator network and cross-stack socket checks inside each actual MCP
container. These additional probes ran before the script removed the stacks.
Observed exit: **0**. No phone was paired, no MCP tool was called, and the
existing REST send check used only a fake group and `dry_run=true`.

| #642 acceptance | Evidence |
| --- | --- |
| Off by default; private operator client can reach health | Proxy-only sockets refused; independent private-network peer got health 200 for both aliases |
| Proxy network cannot reach operator or REST | Independent proxy peer got ECONNREFUSED on 8080, 8090 and 8091 for both instances |
| No data plane on operator; reciprocal token separation | Existing real HTTP deny tests and seven Compose deny paths; private peer got 401 with absent, bridge or MCP token |
| Unsafe startup refused | `TestOperatorConfigRefusesUnsafeBindsTokensAndPorts` and token-file tests pass in the full Go/race gate |
| MCP reads only through operator; admin loopback | Private peer got transcription/usage 200 through the bridge; actual admin/socket and Go/Python forwarding tests pass, with no admin route on MCP or published admin port |
| Two aliases and cross-stack proxy isolation | Private peer reached each alias; actual MCP A→B and B→A proxy sockets all returned ECONNREFUSED |
| Four env docs and operator network layout | Env-documentation contracts pass; CONFIGURATION/DOCKER/operator override document the private listener and bridge-only operator secret |

The first combined run failed before MCP startup because the earlier proxy
smoke selected operator port 8091, now reserved for the loopback admin. The
smoke now uses the operator default 8090. The unchanged runtime then passed
the complete run; the failed receipt was preserved. No port/auth boundary was
relaxed. The Linux proxy/docs contracts passed again: 56 passed, 7 Docker CLI
cases skipped; the 21 native Compose/docs contracts cover CLI parsing.

Selected output, with no credentials:

```text
wamcp-proxy-63096-operator-a:8090 health -> 200 from independent operator-network container
wamcp-proxy-63096-operator-a:8090 transcription/usage -> 200 from independent operator-network container
wamcp-proxy-63096-operator-a: missing/data-plane/MCP token -> 401
wamcp-proxy-63096-operator-b:8090 health -> 200 from independent operator-network container
wamcp-proxy-63096-operator-b:8090 transcription/usage -> 200 from independent operator-network container
wamcp-proxy-63096-operator-b: missing/data-plane/MCP token -> 401
actual stack -> wamcp-proxy-63096-b:8080 ECONNREFUSED
actual stack -> wamcp-proxy-63096-b:8090 ECONNREFUSED
actual stack -> wamcp-proxy-63096-b:8091 ECONNREFUSED
actual stack -> wamcp-proxy-63096-a:8080 ECONNREFUSED
actual stack -> wamcp-proxy-63096-a:8090 ECONNREFUSED
actual stack -> wamcp-proxy-63096-a:8091 ECONNREFUSED
proxy smoke -> PASS (two projects, both override combinations, unpaired)
```

The operator peer used `urllib.request` with proxies disabled, a five-second
timeout, and the operator bearer for GET health/usage on both private aliases;
it asserted unpaired health and zero initial usage. It required HTTP 401 for
each absent/bridge/MCP credential. Each cross-stack probe used
`socket.create_connection((other_proxy_alias, port), timeout=5)` for ports
8080/8090/8091 and accepted only `errno.ECONNREFUSED`; DNS errors, timeouts or
an open socket failed the run. Ordinary initialize from the independent proxy
peer still returned 200/session/serverInfo, and missing/other-instance MCP
tokens returned 401 in both modes.

After the script completed, independent Docker container, volume and network
listings for `wamcp-proxy-63096` were empty. The MCP/bridge namespace still
shares port 8000 on joined networks; separating the agent plane is #678,
outside #642's operator-route acceptance.
