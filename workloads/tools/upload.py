#!/usr/bin/env python3
"""Upload a CGPU workload's outputs to the CGPU API.

Runs inside the workload container after the workload exits (see the shared Job template).
Every regular file under the outputs directory is sent to
    PUT $CGPU_API_URL/internal/v1/jobs/$CGPU_JOB_ID/artifacts?path=<relative path>
with the per-job bearer token. Standard library only, so it works in any Python image.
"""
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


def upload(api, job_id, token, root, rel):
    full = os.path.join(root, rel)
    size = os.path.getsize(full)
    url = "%s/internal/v1/jobs/%s/artifacts?path=%s" % (
        api.rstrip("/"), urllib.parse.quote(job_id), urllib.parse.quote(rel))
    for attempt in range(1, 4):
        try:
            with open(full, "rb") as f:
                req = urllib.request.Request(url, data=f, method="PUT", headers={
                    "Authorization": "Bearer " + token,
                    "Content-Type": "application/octet-stream",
                    "Content-Length": str(size),
                })
                with urllib.request.urlopen(req, timeout=300) as resp:
                    resp.read()
            print("[CGPU-UPLOAD] %s (%d bytes) uploaded" % (rel, size), flush=True)
            return True
        except (urllib.error.URLError, OSError) as e:
            print("[CGPU-UPLOAD] %s attempt %d failed: %s" % (rel, attempt, e), file=sys.stderr, flush=True)
            time.sleep(2 * attempt)
    return False


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else "/outputs"
    api, job_id, token = (os.environ.get(k) for k in ("CGPU_API_URL", "CGPU_JOB_ID", "CGPU_UPLOAD_TOKEN"))
    if not (api and job_id and token):
        print("[CGPU-UPLOAD] CGPU_API_URL / CGPU_JOB_ID / CGPU_UPLOAD_TOKEN not set; skipping", file=sys.stderr)
        return 1
    files = []
    for dirpath, _, names in os.walk(root):
        for n in names:
            full = os.path.join(dirpath, n)
            if os.path.isfile(full) and not n.startswith("."):
                files.append(os.path.relpath(full, root).replace(os.sep, "/"))
    if not files:
        print("[CGPU-UPLOAD] no output files in %s" % root, flush=True)
        return 0
    failed = [rel for rel in sorted(files) if not upload(api, job_id, token, root, rel)]
    if failed:
        print("[CGPU-UPLOAD] failed: %s" % ", ".join(failed), file=sys.stderr, flush=True)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
