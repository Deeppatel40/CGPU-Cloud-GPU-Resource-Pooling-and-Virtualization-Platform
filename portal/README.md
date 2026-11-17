# CGPU Portal (Issue #13 / B3)

Next.js 15 (App Router) + React 19 + TypeScript web portal for CGPU. Use it to submit GPU
training jobs, follow their status and logs, download results, and inspect the pooled GPUs.
There are no UI libraries; styling is plain CSS in `app/globals.css` (dark by default, light
when the OS prefers it).

The browser only talks to the portal's own origin. `next.config.ts` rewrites
`/api/v1/:path*` to `${CGPU_API_URL}/api/v1/:path*` (default `http://localhost:8080`, the Go
API from issue #12). Live logs use Server-Sent Events through a small route handler
(`/api/portal/log-stream/{job_id}`). It forwards the stream unbuffered and uncompressed.

## Run

Requires Node 20+ (tested on Node 24).

```bash
cd portal
npm install
```

### Against the Python mock

The mock serves the same `/api/v1/...` paths and takes its port as a positional argument
(default 8080):

```bash
python3 api/mocks/server.py 8000                    # from the repo root
CGPU_API_URL=http://localhost:8000 npm run dev      # in portal/ -> http://localhost:3000
```

Mock limitations:

- It has no `GET /api/v1/workloads`, so the portal uses its built-in catalog
  (`pytorch-resnet18-cifar10`).
- It answers `logs?stream=true` with JSON, so the log viewer falls back to polling every 3 s.
- `/artifacts` always returns the completed job's files.

To see each scenario, open these job IDs:

| Scenario | URL |
| --- | --- |
| Queued (WAITING_FOR_CAPACITY) | `/jobs/job-d41a7e23-7890-41bc-9602-0a12e8b23f99` |
| Running, telemetry unavailable | `/jobs/job-87fc3182-1100-4b34-8c99-90de5b1287c2` |
| Failed (CUDA_OUT_OF_MEMORY) | `/jobs/job-f1503921-bc74-4b52-9cb0-1845bb08a120` |
| Succeeded + checkpoint download | `/jobs/job-9e2b14f8-45a1-432c-b58a-6b5832a8e412` |
| Submit success | `/submit`, keep the defaults, then **Submit job** |
| Submit queued | `/submit` with VRAM ≥ 4096 MiB (the mock answers WAITING_FOR_CAPACITY) |
| Invalid input (client-side) | `/submit` with e.g. name `bad name!` or VRAM `0` |
| Invalid input (server 400 `details`) | `/submit?novalidate=1` with VRAM `-1` (turns off client checks for the demo) |

### Against the real Go API

```bash
# dev
CGPU_API_URL=http://localhost:8080 npm run dev

# production (binds 0.0.0.0:3000)
CGPU_API_URL=http://localhost:8080 npm run build
npm start
```

> **`CGPU_API_URL` is read at build time.** Next.js writes rewrites into the build output, so
> set the variable for `npm run build` (and `npm run dev`). Setting it only for `npm start`
> has no effect. The default `http://localhost:8080` matches the planned deployment, where
> the API and the portal run on the same machine. The footer shows which API the build
> targets.

### Checks

```bash
npm run typecheck   # tsc --noEmit (also available as `npm run lint`)
npm run build
```

## Page map

| Route | What it shows |
| --- | --- |
| `/` | Overview: pool tiles (nodes, capacity, reserved, free, measured) and recent jobs. Refreshes every 5 s. |
| `/submit` | Submission form. Pick a workload from `GET /api/v1/workloads` (or the built-in fallback), fill in its typed parameters, set a VRAM request (input plus slider, warns when it exceeds the largest free slice right now), priority and timeout. Client validation mirrors the server rules, and server 400 `details[]` are mapped onto the matching fields. |
| `/jobs` | Job history: status and workload filters, pagination (`?status=&workload_id=&page=`). Refreshes every 5 s. |
| `/jobs/[id]` | Job detail: status and sub-status; pending reason (plain-language explanation plus the code, queue position, requested vs. free VRAM, estimated wait); failure reason (explanation, code, exit code, error message, collapsible stack trace); placement (worker node, GPU ID and index, reserved VRAM, enforcement mode); measured telemetry or "unavailable"; execution metrics; artifacts with download links; logs (live SSE while running, polling fallback). Cancel button calls `DELETE`. Refreshes every 3 s until SUCCEEDED/FAILED. |
| `/gpus` | GPU inventory per node and GPU, with separate labels for **capacity**, **allocatable** (capacity − 512 MiB driver reserve), **reserved** (scheduler allocations), **free**, and **measured used** (telemetry; shown as "unavailable" when missing, never 0). OFFLINE nodes are flagged in red. Each GPU lists its active allocations with links to the jobs. Refreshes every 5 s. |
| `/api/portal/log-stream/[id]` | Route handler that proxies `GET /api/v1/jobs/{id}/logs?stream=true` as `text/event-stream`. |

## Behaviour notes

- **Idempotent submission.** Each submission gets a UUIDv4 `request_id`, sent in the body
  and as `X-Request-Id`. Automatic retries on network errors and 502/503/504 reuse it, and so
  does the manual "Retry submission" button. Any edit to the form clears it, because the
  edited form is a new submission. Double-clicks are ignored while a request is in flight.
  A 201 response shows "Job submitted successfully", and a 200 shows "Submission already
  received" (an existing job was returned). If `crypto.randomUUID` is unavailable (the portal
  opened over plain HTTP on a LAN IP), the ID is generated from `crypto.getRandomValues`.
- **Live logs.** For a RUNNING job the viewer opens `EventSource` against the route handler.
  Each `data:` event is a `LogLine` JSON object. On `event: end` it closes the stream and
  loads the final JSON tail. If the stream can't be opened or drops, it polls
  `logs?tail=500` every 3 s.
- **Telemetry.** A missing `telemetry` object, `status: "UNAVAILABLE"`, or an offline node
  displays as *unavailable* along with the reason. Readings are never shown as 0.

## Layout

```
app/                     routes (thin server components) + globals.css
app/api/portal/log-stream/[id]/route.ts   SSE proxy
components/              client components (SubmitForm, JobList, JobDetail, LogViewer,
                         ArtifactList, GpuInventory, Overview, Nav, ui)
lib/api.ts               typed fetch client + error type, retry, workload fallback
lib/validation.ts        client rules + server-error mapping
lib/usePolling.ts        polling hook (no overlap, keeps last good data)
lib/reasons.ts           human-readable pending/failure reason texts
types/api.ts             shared API contract types (extended with workload/artifact types)
```
