#!/usr/bin/env python3
"""
CGPU Mock API Server
Provides a standalone, zero-dependency HTTP mock server serving mock responses
for decoupled Next.js portal development and backend integration testing.

Usage:
    python api/mocks/server.py [--port 8080]
"""

import os
import sys
import json
import uuid
from pathlib import Path
from http.server import HTTPServer, BaseHTTPRequestHandler

MOCKS_DIR = Path(__file__).resolve().parent

def load_mock(filename):
    filepath = MOCKS_DIR / filename
    with open(filepath, "r", encoding="utf-8") as f:
        return json.load(f)

class CGPUMockHandler(BaseHTTPRequestHandler):
    def _send_cors_headers(self):
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type, X-Request-Id, Authorization")

    def do_OPTIONS(self):
        self.send_response(204)
        self._send_cors_headers()
        self.end_headers()

    def _reply_json(self, status_code, body):
        self.send_response(status_code)
        self.send_header("Content-Type", "application/json")
        self._send_cors_headers()
        self.end_headers()
        self.wfile.write(json.dumps(body, indent=2).encode("utf-8"))

    def do_GET(self):
        path = self.path.split("?")[0].rstrip("/")

        # 1. GPU Inventory
        if path == "/api/v1/gpus":
            data = load_mock("07_gpu_inventory.json")
            self._reply_json(data["status_code"], data["data"])
            return

        # 2. Job History
        if path == "/api/v1/jobs":
            data = load_mock("08_job_history.json")
            self._reply_json(data["status_code"], data["data"])
            return

        # 3. Logs
        if path.startswith("/api/v1/jobs/") and path.endswith("/logs"):
            data = load_mock("09_job_logs.json")
            self._reply_json(data["status_code"], data["data"])
            return

        # 4. Artifacts List
        if path.startswith("/api/v1/jobs/") and path.endswith("/artifacts"):
            job_mock = load_mock("06_completed_job_checkpoint.json")
            self._reply_json(200, {
                "job_id": job_mock["data"]["job_id"],
                "artifacts": job_mock["data"]["artifacts"]
            })
            return

        # 5. Artifact Download
        if "/artifacts/" in path and path.endswith("/download"):
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Disposition", "attachment; filename=\"checkpoint.pt\"")
            self._send_cors_headers()
            self.end_headers()
            self.wfile.write(b"PK\x03\x04CGPU_MOCK_CHECKPOINT_BINARY_STREAM")
            return

        # 6. Specific Job Status
        if path.startswith("/api/v1/jobs/"):
            job_id = path.split("/")[-1]
            if "fail" in job_id or "oom" in job_id or "f1503921" in job_id:
                mock = load_mock("05_job_failure_oom.json")
            elif "telemetry" in job_id or "87fc3182" in job_id:
                mock = load_mock("04_unavailable_telemetry.json")
            elif "wait" in job_id or "capacity" in job_id or "d41a7e23" in job_id:
                mock = load_mock("02_waiting_for_capacity.json")
            else:
                mock = load_mock("06_completed_job_checkpoint.json")
            self._reply_json(mock["status_code"], mock["data"])
            return

        # Default 404
        self._reply_json(404, {
            "error_code": "NOT_FOUND",
            "message": f"Endpoint {path} not found on CGPU mock server.",
            "timestamp": "2026-11-14T11:00:00Z"
        })

    def do_POST(self):
        path = self.path.split("?")[0].rstrip("/")

        if path == "/api/v1/jobs":
            content_length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(content_length)
            try:
                payload = json.loads(body.decode("utf-8")) if body else {}
            except json.JSONDecodeError:
                self._reply_json(400, {
                    "error_code": "INVALID_JSON",
                    "message": "Malformed JSON payload.",
                    "timestamp": "2026-11-14T11:00:00Z"
                })
                return

            vram = payload.get("requested_vram_mib", 2048)
            # Scenario 3: Validation Error
            if vram <= 0 or not payload.get("workload_id") or not payload.get("request_id"):
                mock = load_mock("03_invalid_request.json")
                self._reply_json(mock["status_code"], mock["error"])
                return

            # Scenario 2: Waiting for capacity (if requested >= 4096 MiB)
            if vram >= 4096:
                mock = load_mock("02_waiting_for_capacity.json")
                self._reply_json(201, mock["data"])
                return

            # Scenario 1: Submission Success
            mock = load_mock("01_submission_success.json")
            resp_data = dict(mock["data"])
            resp_data["job_id"] = f"job-{uuid.uuid4()}"
            resp_data["request_id"] = payload.get("request_id", resp_data["request_id"])
            resp_data["name"] = payload.get("name", resp_data["name"])
            resp_data["workload_id"] = payload.get("workload_id", resp_data["workload_id"])
            resp_data["requested_vram_mib"] = vram
            self._reply_json(201, resp_data)
            return

        self._reply_json(404, {"error_code": "NOT_FOUND", "message": "Endpoint not found."})

    def do_DELETE(self):
        path = self.path.split("?")[0].rstrip("/")
        if path.startswith("/api/v1/jobs/"):
            job_id = path.split("/")[-1]
            self._reply_json(200, {
                "job_id": job_id,
                "status": "FAILED",
                "failure_reason": "CANCELLED_BY_USER",
                "message": "Job successfully cancelled and resources released."
            })
            return
        self._reply_json(404, {"error_code": "NOT_FOUND", "message": "Endpoint not found."})

def run(port=8080):
    server_address = ("", port)
    httpd = HTTPServer(server_address, CGPUMockHandler)
    print(f"[CGPU Mock Server] Listening on http://localhost:{port}/api/v1")
    print(f"[CGPU Mock Server] Loaded mocks from {MOCKS_DIR}")
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print("\n[CGPU Mock Server] Shutting down.")

if __name__ == "__main__":
    port = 8080
    if len(sys.argv) > 1 and sys.argv[1].isdigit():
        port = int(sys.argv[1])
    run(port)

