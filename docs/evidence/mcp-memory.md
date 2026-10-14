# MCP idle memory measurement (#704)

Local Docker Desktop, Linux amd64, Python 3.13, MCP SDK 2.3.0. The daemon reports
cgroup **v1**. The coordinator authorized v1 here: `memory.usage_in_bytes`,
`total_rss` (anonymous) and `total_cache`, kept separately. The versioned script
also reads `memory.current`, `anon` and `file` on cgroup v2 without configuration
changes. RSS includes shared/library pages and is not interchangeable with
cgroup anonymous memory. The tiny transient `cat` sampler is included in cgroup
current; Python probe clients exit before sampling and do not inflate anon.

The script builds no images itself; supply the image under test. It creates a
fresh private writable container layer with 1000 synthetic messages and one
fake group, permits no external network, launches the actual `main.py`, checks
the first real `list_chats` result, then makes 100 sequential calls via the MCP
SDK HTTP client in a new session. Each sample follows three seconds of quiet.
Only its own registered container ID is removed, including on failure.

| Image/experiment | Stage | Current MiB | Anonymous MiB | Threads |
|---|---|---:|---:|---:|
| baseline | idle | 74.11 | 69.45 | 2 |
| baseline | first call | 75.70 | 70.73 | 4 |
| baseline | 100 calls | 76.28 | 71.39 | 4 |
| baseline, arena max 2 | idle | 74.05 | 69.32 | 2 |
| baseline, arena max 2 | first call | 77.45 | 72.27 | 4 |
| baseline, arena max 2 | 100 calls | 77.82 | 72.90 | 4 |
| lazy Pillow experiment | idle | 73.68 | 68.73 | 2 |
| lazy Pillow experiment | first call | 75.66 | 70.51 | 4 |
| lazy Pillow experiment | 100 calls | 75.96 | 70.83 | 4 |
| final memory/proxy PR | idle | 77.13 | 71.30 | 2 |
| final memory/proxy PR | first call | 80.20 | 74.57 | 4 |
| final memory/proxy PR | 100 calls | 80.50 | 74.75 | 4 |

Raw files: [baseline](mcp-memory-before.json), [allocator](mcp-memory-arena2.json),
[lazy Pillow experiment](mcp-memory-lazy-pillow-experiment.json). The experiment
deferred `PIL.Image` into the renderer and was reverted: less than 1 MiB of idle
anonymous memory did not justify a runtime change. MALLOC_ARENA_MAX=2 also did
not improve this bounded workload, so the Dockerfile allocator default stays.

The separate import process measures incremental **RSS**, not causal cgroup
anonymous attribution. In the baseline profile, the MCP server SDK import adds
51.6 MiB, OAuth adds 4.6 MiB, and our registered tools/imports add 18.9 MiB.
Pillow is the only eager optional decoder; deferring it saves roughly 1.8 MiB
RSS but only 0.7 MiB idle anonymous. PDFium, openpyxl and DOCX already load on
demand (their additional baseline import RSS is about 2.1, 7.3 and 2.3 MiB). The SDK's
schema/Pydantic/HTTP imports are the largest measured floor. No dependency or
SDK refactor was attempted.

One uvicorn process/worker serves HTTP.
Two threads are present idle and four after synchronous tools run (AnyIO's
worker pool plus HTTP runtime); the observed count stays four across the batch.
No extra workers were added. The optional admin listener/transcription worker
are off in this workload; enabling them needs another measurement.

Per-process caches: identity is one entry / 60 s; the name cache now has a 300 s TTL
and 4096-entry bound (expired entries swept on insertion, oldest admission evicted); media directory
names are capped at 16 chats / 5 s; OAuth
introspection is capped at 1024 positive entries / 60 s (off here). These were
inspected in the actual source; the 100-call growth includes schema/session,
thread and name-cache allocations and is only about 1.9 MiB anonymous. The
synthetic workload does not establish a worst-case cache/media workload.
The coordinator authorized a minimal `whatsapp.py` cache correction in this PR.
The repeated 10000-name workload retained 10000 names / 2.84 MiB RSS before,
versus 4096 / 1.03 MiB after; one insertion after TTL retained 10001 names before
and only one after. This reduces growth with many identities, while the idle
floor remains 69-75 MiB anonymous across runs. Both cache regressions failed with the
original `_cache_put` and passed with the correction (49 focused cache/contact
tests green). [Final PR-image output](mcp-memory-final.json) includes this probe.

Recommend **256 MiB** for text/search deployments, leaving over 3x the measured
anonymous floor and room for reclaimable file cache. This is not a safe upper
bound for media decoding: the existing 64-million-pixel image cap alone permits
hundreds of MiB of decoded pixels, and document extraction expands archives.
Use 512 MiB or more and measure actual files/concurrency for those workloads.

Reproduce the baseline/arena comparison:

```sh
docker build -t wamcp-memory:local whatsapp-mcp-server
python scripts/measure-mcp-memory.py --image wamcp-memory:local --output docs/evidence/local.json
python scripts/measure-mcp-memory.py --image wamcp-memory:local --arena 2 --output docs/evidence/local-arena2.json
```
