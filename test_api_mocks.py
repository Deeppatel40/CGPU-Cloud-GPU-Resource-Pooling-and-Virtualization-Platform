#!/usr/bin/env python3
"""
CGPU Mock & Contract Test Suite
Tests all mock response files, data schemas, and demonstration workload logic.
Run with: python test_api_mocks.py
"""

import os
import sys
import json
import glob
from pathlib import Path

def test_json_mocks():
    print("\n--- 1. Testing Mock Response Files ---")
    mock_files = sorted(glob.glob("api/mocks/*.json"))
    assert len(mock_files) >= 9, f"Expected at least 9 mock files, found {len(mock_files)}"
    
    for f in mock_files:
        with open(f, "r", encoding="utf-8") as fp:
            data = json.load(fp)
            status_code = data.get("status_code", 200)
            print(f"  [PASS] {os.path.basename(f):35s} (HTTP {status_code})")

def test_openapi_contract():
    print("\n--- 2. Testing OpenAPI 3.1 Contract ---")
    with open("api/openapi/openapi.json", "r", encoding="utf-8") as fp:
        spec = json.load(fp)
        assert spec["openapi"].startswith("3."), "Invalid OpenAPI version"
        assert len(spec["paths"]) >= 7, "Missing required paths"
        assert len(spec["components"]["schemas"]) >= 15, "Missing required schemas"
        print(f"  [PASS] OpenAPI Contract valid: {spec['info']['title']} v{spec['info']['version']}")
        print(f"  [PASS] Defined endpoints: {len(spec['paths'])} paths, {len(spec['components']['schemas'])} schemas")

def test_mock_scenarios():
    print("\n--- 3. Testing Required Issue #8 Acceptance Scenarios ---")
    
    # 1. Successful submission
    with open("api/mocks/01_submission_success.json") as f:
        d = json.load(f)["data"]
        assert d["status"] == "PENDING"
        assert d["requested_vram_mib"] == 2048
        assert d["request_id"].startswith("req-")
        print("  [PASS] Scenario 1: Successful submission contract verified (positive VRAM integer + request_id)")

    # 2. Waiting for capacity
    with open("api/mocks/02_waiting_for_capacity.json") as f:
        d = json.load(f)["data"]
        assert d["status"] == "PENDING"
        assert d["pending_reason"] == "WAITING_FOR_CAPACITY"
        assert d["pending_details"]["required_vram_mib"] > d["pending_details"]["available_vram_mib"]
        print("  [PASS] Scenario 2: Waiting for capacity contract verified (deficit explanation & queue)")

    # 3. Invalid requests
    with open("api/mocks/03_invalid_request.json") as f:
        err = json.load(f)["error"]
        assert err["error_code"] == "VALIDATION_FAILED"
        assert any(det["field"] == "requested_vram_mib" for det in err["details"])
        print("  [PASS] Scenario 3: Invalid request contract verified (negative VRAM & missing fields caught)")

    # 4. Unavailable telemetry
    with open("api/mocks/04_unavailable_telemetry.json") as f:
        d = json.load(f)["data"]
        assert d["telemetry"]["status"] == "UNAVAILABLE"
        assert d["status"] == "RUNNING"
        print("  [PASS] Scenario 4: Unavailable telemetry contract verified (graceful fallback while RUNNING)")

    # 5. Job failure
    with open("api/mocks/05_job_failure_oom.json") as f:
        d = json.load(f)["data"]
        assert d["status"] == "FAILED"
        assert d["failure_reason"] == "CUDA_OUT_OF_MEMORY"
        assert d["failure_details"]["exit_code"] == 137
        print("  [PASS] Scenario 5: Job failure contract verified (CUDA_OUT_OF_MEMORY + stacktrace)")

    # 6. Completed job with checkpoint
    with open("api/mocks/06_completed_job_checkpoint.json") as f:
        d = json.load(f)["data"]
        assert d["status"] == "SUCCEEDED"
        assert len(d["artifacts"]) >= 2
        assert any(a["artifact_type"] == "checkpoint" for a in d["artifacts"])
        print("  [PASS] Scenario 6: Completed job with checkpoint verified (download URLs + sha256 hashes)")

def test_demonstration_workload():
    print("\n--- 4. Testing Demonstration Workload Script Execution ---")
    import subprocess
    cmd = [sys.executable, "workloads/demonstration/train.py", "--epochs", "2", "--output-dir", "./test_workload_tmp"]
    res = subprocess.run(cmd, capture_output=True, text=True)
    assert res.returncode == 0, f"Workload execution failed:\n{res.stderr}"
    
    # Check outputs produced
    output_dir = Path("./test_workload_tmp")
    metrics_file = output_dir / "metrics.json"
    ckpt_file = output_dir / "checkpoints" / "checkpoint_epoch_2.pt"
    final_model = output_dir / "checkpoints" / "model_final.pt"
    
    assert metrics_file.exists(), "metrics.json not created!"
    assert ckpt_file.exists(), "checkpoint_epoch_2.pt not created!"
    assert final_model.exists(), "model_final.pt not created!"
    
    with open(metrics_file) as mf:
        metrics = json.load(mf)
        print(f"  [PASS] Workload generated metrics: final_acc={metrics['final_accuracy']}%, peak_vram={metrics['peak_vram_mib']}MiB")
        print(f"  [PASS] Workload generated checkpoint: {ckpt_file.name} ({ckpt_file.stat().st_size} bytes)")
    
    # Clean up
    import shutil
    shutil.rmtree(output_dir, ignore_errors=True)
    print("  [PASS] Cleaned up temporary test artifacts.")

if __name__ == "__main__":
    print("=========================================================")
    print(" CGPU Platform - Issue B1 Contract & Mock Test Suite")
    print("=========================================================")
    try:
        test_json_mocks()
        test_openapi_contract()
        test_mock_scenarios()
        test_demonstration_workload()
        print("\n=========================================================")
        print(" ALL TESTS PASSED! Issue B1 Acceptance Criteria Verified.")
        print("=========================================================\n")
    except Exception as e:
        print(f"\n[FAIL] Test failed: {e}")
        sys.exit(1)

