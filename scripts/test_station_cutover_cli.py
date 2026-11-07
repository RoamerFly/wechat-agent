#!/usr/bin/env python3
"""Exercise the packaged Windows station cutover with synthetic local data only.

The test imports a legacy identity, model settings, project home, speech voice,
singing voice, and the four built-in creation tools.  It then starts the
packaged station and verifies the imported state over its loopback HTTP API.
It never calls a model, WeChat, or a neural inference service.
"""

from __future__ import annotations

import argparse
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import queue
import re
import subprocess
import sys
import tempfile
import threading
import urllib.request
from typing import Any


CREATE_NO_WINDOW = getattr(subprocess, "CREATE_NO_WINDOW", 0)


def write(path: Path, value: bytes | dict[str, Any]) -> Path:
    """Create one synthetic fixture file and return it."""
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = value if isinstance(value, bytes) else json.dumps(value, ensure_ascii=False).encode("utf-8")
    path.write_bytes(payload)
    return path


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path, help="Path to agent-home-station-server.exe")
    return parser.parse_args()


def station_command(binary: Path, arguments: list[str]) -> dict[str, Any]:
    """Run a station cutover command and require non-secret JSON output."""
    result = subprocess.run(
        [str(binary), *arguments],
        capture_output=True,
        encoding="utf-8",
        timeout=30,
        creationflags=CREATE_NO_WINDOW,
    )
    assert result.returncode == 0, result.stderr.strip() or result.stdout.strip()
    assert "synthetic-private-key" not in result.stdout
    return json.loads(result.stdout)


def confirm(binary: Path, arguments: list[str]) -> dict[str, Any]:
    """Preview, apply, then prove an idempotent legacy import."""
    plan = station_command(binary, arguments)
    assert plan["status"] == "preview", plan
    applied = station_command(binary, [*arguments, "--digest", plan["digest"]])
    assert applied["status"] == "applied", applied
    repeated = station_command(binary, [*arguments, "--digest", plan["digest"]])
    assert repeated["status"] == "applied", repeated
    return plan


def create_legacy_settings(base: Path, data: Path, binary: Path) -> tuple[str, str, dict[Path, str]]:
    old = base / "old-admin"
    old.mkdir()
    fish_id = "fish-" + "a" * 48
    owner_id = "owner-" + "b" * 48
    write(old / "fish-identity.json", {
        "version": 3,
        "fish_id": fish_id,
        "bindings": [],
        "local_owner": {"id": owner_id, "revision": 1, "home_access": True},
    })
    write(old / "config.json", {
        "provider": "deepseek",
        "model": "deepseek-flash",
        "base_url": "https://model.invalid",
        "api_key": "synthetic-private-key",
        "thinking_enabled": True,
        "reasoning_effort": "low",
        "companion": {"persona": "synthetic continuous companion"},
    })
    source_hashes = {path: sha256(path) for path in old.iterdir()}
    arguments = ["--data", str(data), "--legacy-settings-source", str(old)]
    preview = station_command(binary, arguments)
    assert not data.exists(), "preview must not create the target station"
    applied = station_command(binary, [*arguments, "--legacy-settings-digest", preview["digest"]])
    assert applied["status"] == "applied", applied
    return fish_id, owner_id, source_hashes


def import_legacy_home(base: Path, data: Path, binary: Path) -> Path:
    project = base / "old-project"
    persistent = write(project / "persistent.py", b"print('synthetic continuing project')\n")
    arguments = [
        "--data", str(data), "--legacy-home-source", str(project),
        "--legacy-home-target", "projects/continued",
    ]
    preview = station_command(binary, arguments)
    assert preview["status"] == "preview", preview
    applied = station_command(binary, [*arguments, "--legacy-home-digest", preview["digest"]])
    assert applied["status"] == "applied", applied
    return persistent


def import_speech(base: Path, data: Path, package: Path, binary: Path, media: Path) -> dict[str, Any]:
    speech = base / "old-speech"
    entries: list[dict[str, Any]] = []
    for filename, kind, contents in [
        ("voice.ckpt", "gpt", b"gpt"),
        ("voice.pth", "sovits", b"sovits"),
        ("sample.wav", "reference", b"sample"),
        ("sample.txt", "prompt", b"synthetic reference"),
    ]:
        item = write(speech / "files" / filename, contents)
        entries.append({
            "file": filename, "filename": filename, "kind": kind,
            "sha256": sha256(item), "bytes": item.stat().st_size,
        })
    write(speech / "catalog.json", {"items": [{
        "id": "legacy-speaking", "name": "Synthetic voice", "engine": "GPT-SoVITS",
        "status": "已归档，待运行验证", "weights": entries[:2], "references": entries[2:],
    }]})
    runner = package / "runtimes" / "windows-station-speech" / "gpt_sovits.py"
    deployment: dict[str, Any] = {
        "schema_version": 1, "enabled": True,
        "python": str(package / "runtimes" / "windows-python-cli" / "python.exe"),
        "media": str(media), "runner": str(runner), "runner_sha256": sha256(runner),
        "device": "cpu", "voices": [],
    }
    for name in ("project", "base_models", "nltk_data"):
        directory = base / f"speech-{name}"
        directory.mkdir()
        deployment[name] = str(directory)
    plan = write(base / "speech-plan.json", {
        "schema_version": 1, "library": str(speech), "allow_unowned": True,
        "voice_ids": ["legacy-speaking"], "prompt_language": "all_zh",
        "text_language": "auto", "deployment": deployment,
    })
    return confirm(binary, ["legacy-speech", "--data", str(data), "--plan", str(plan)])


def import_rvc(base: Path, data: Path, binary: Path, media: Path) -> tuple[dict[str, Any], Path, Path]:
    resources = base / "rvc-resources"
    public = resources / "public_workspace"
    (resources / "pri_workspace").mkdir(parents=True)
    rvc_project = public / "projects" / "rvc"
    for relative in ("assets/hubert_base/pytorch_model.bin", "assets/rmvpe/rmvpe.pt", "infer/vc/modules.py"):
        write(rvc_project / relative, b"synthetic dependency")
    rvc_python = write(public / "runtimes" / "python" / "python.exe", b"synthetic interpreter")
    archive = base / "old-rvc"
    model = write(archive / "weights" / "voice.pth", b"synthetic rvc model")
    index = write(archive / "weights" / "voice.index", b"synthetic rvc index")
    configuration = write(archive / "voice.config", {
        "enabled": True, "engine": "rvc", "profile": "singing-model-label",
        "python": str(rvc_python), "rvc_python": str(rvc_python),
        "runtime": str(rvc_python.parent), "rvc_project": str(rvc_project),
        "rvc_model": str(model), "rvc_index": str(index),
        "rvc_model_sha256": sha256(model), "rvc_index_sha256": sha256(index), "rvc_device": "cpu",
    })
    write(archive / "manifests" / "private-singing.json", {"configuration": str(configuration)})
    write(archive / "catalog.json", {"items": [{
        "id": "legacy-singing", "name": "Synthetic singing voice", "engine": "rvc",
        "profile_id": "private-singing", "configuration": str(configuration),
    }]})
    plan = write(base / "rvc-plan.json", {
        "schema_version": 1, "library": str(archive), "allow_unowned": True,
        "voice_ids": ["legacy-singing"], "deployment": {
            "schema_version": 1, "enabled": True, "workspace": str(resources),
            "media": str(media), "port": 6198, "profiles": [],
        },
    })
    return confirm(binary, ["legacy-rvc", "--data", str(data), "--plan", str(plan)]), model, configuration


def write_creation_tools(base: Path, data: Path, media: Path) -> None:
    """Prepare synthetic ComfyUI and YuE registrations owned by the default workspace."""
    runtime = base / "creation-runtime"
    python = write(runtime / "python.exe", b"synthetic interpreter")
    entry = write(runtime / "ComfyUI" / "main.py", b"synthetic entry")
    (runtime / "frontend").mkdir()
    models, workflows = base / "models", base / "workflows"
    models.mkdir()
    workflows.mkdir()
    pins: dict[str, str] = {}
    for mode, filename in (("original", "audio_yue2_text_to_music.json"), ("cover", "audio_yue2_music_cover.json")):
        pins[mode] = sha256(write(workflows / filename, {"synthetic": mode}))
    graph = write(workflows / "synthetic.json", {
        "1": {"class_type": "EmptyImage", "inputs": {"width": 32, "height": 24, "batch_size": 1, "color": 0}},
        "9": {"class_type": "SaveImage", "inputs": {"images": ["1", 0], "filename_prefix": "synthetic"}},
    })
    write(workflows / "registry.json", {"schema_version": 1, "comfyui_version": "0.37.2", "workflows": [{
        "id": "synthetic", "label": "Synthetic image", "graph": "synthetic.json",
        "sha256": sha256(graph), "save_node": "9", "parameters": [],
    }]})
    for name, policy, port in (("comfyui", "station-comfyui-v1", 8190), ("yue", "station-yue-v1", 8191)):
        manifest = write(data / f"{name}-manifest.json", {
            "schema_version": 1, "policy": policy,
            "files": {"python.exe": sha256(python), "ComfyUI/main.py": sha256(entry)},
        })
        registration: dict[str, Any] = {
            "schema_version": 1, "enabled": True, "runtime": str(runtime), "python": "python.exe",
            "entry": "ComfyUI/main.py", "frontend": "frontend", "manifest": str(manifest),
            "models": str(models), "workflows": str(workflows), "port": port, "device": "cpu",
        }
        if name == "yue":
            registration.update({"workflow_pins": pins, "ffmpeg": str(media / "ffmpeg.exe"), "ffmpeg_sha256": sha256(media / "ffmpeg.exe")})
        write(data / f"station-{name}.json", registration)


def verify_http(binary: Path, data: Path, fish_id: str, owner_id: str, persistent: Path,
                speaking: dict[str, Any], singing: dict[str, Any], install: bool) -> None:
    """Start the packaged server and check imports through actual local HTTP endpoints."""
    process = subprocess.Popen(
        [str(binary), "--desktop", "--data", str(data), "--listen", "127.0.0.1:0"],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        env={**os.environ, "AHS_ADMIN_PASSWORD": "synthetic-cutover-owner"},
        creationflags=CREATE_NO_WINDOW,
    )
    try:
        assert process.stdout is not None
        lines: queue.Queue[bytes] = queue.Queue()
        threading.Thread(target=lambda: lines.put(process.stdout.readline()), daemon=True).start()
        endpoint = re.search(rb"127\.0\.0\.1:\d+", lines.get(timeout=25))
        assert endpoint, "station did not announce a loopback endpoint"
        origin = "http://" + endpoint.group().decode("ascii")
        opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),
        )

        def request(path: str, method: str = "GET", payload: dict[str, Any] | None = None, csrf: str | None = None) -> dict[str, Any]:
            headers = {"Content-Type": "application/json", "Origin": origin}
            if csrf:
                headers["X-CSRF-Token"] = csrf
            request_data = json.dumps(payload).encode("utf-8") if payload is not None else None
            request_object = urllib.request.Request(origin + path, headers=headers, method=method, data=request_data)
            with opener.open(request_object, timeout=15) as response:
                raw = response.read()
            assert b"synthetic-private-key" not in raw
            return json.loads(raw)

        csrf = request("/api/login", "POST", {"password": "synthetic-cutover-owner"})["csrf"]
        state = request("/api/local/home")
        assert state["fish_id"] == fish_id and state["owner_id"] == owner_id and state["home_access"]
        copied = Path(state["directory"]) / "projects" / "continued" / "persistent.py"
        assert copied.read_bytes() == persistent.read_bytes()
        model = request("/api/local/model")
        assert model["model"] == "deepseek-flash" and model["key_configured"]
        assert model["thinking_enabled"] and model["reasoning_effort"] == "low"

        def tools() -> dict[str, dict[str, Any]]:
            return {tool["id"]: tool for tool in request("/api/local/project-tools")["tools"]}

        initial = tools()
        for tool_id in ("station-speech", "rvc", "comfyui", "yue"):
            tool = initial[tool_id]
            assert tool["catalog_available"] and tool["runtime_ready"], tool
            if install:
                request(f"/api/local/project-tools/{tool_id}", "PUT", {"action": "install", "approve_artifacts": True}, csrf)
                request(f"/api/local/project-tools/{tool_id}", "PUT", {"action": "enable", "revision": tools()[tool_id]["revision"]}, csrf)
            assert tools()[tool_id]["enabled"], tools()[tool_id]
        assert tools()["station-speech"]["speech_voices"] == speaking["voices"]
        assert tools()["rvc"]["voice_profiles"] == singing["profiles"]
    finally:
        try:
            if process.poll() is None:
                assert process.stdin is not None
                process.stdin.write(b"stop\n")
                process.stdin.flush()
            assert process.wait(timeout=20) == 0, "station did not exit cleanly"
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=10)
            for stream in (process.stdin, process.stdout, process.stderr):
                if stream is not None:
                    stream.close()


def main() -> int:
    arguments = parse_args()
    binary = arguments.binary.resolve()
    assert binary.is_file(), f"server binary does not exist: {binary}"
    package = binary.parent
    required_runner = package / "runtimes" / "windows-station-speech" / "gpt_sovits.py"
    assert required_runner.is_file(), f"packaged speech runner does not exist: {required_runner}"

    with tempfile.TemporaryDirectory(prefix="ahs-cutover-cli-") as temporary:
        base = Path(temporary)
        data = base / "new-station"
        fish_id, owner_id, source_hashes = create_legacy_settings(base, data, binary)
        persistent = import_legacy_home(base, data, binary)
        media = base / "media"
        write(media / "ffmpeg.exe", b"synthetic decoder")
        write(media / "ffprobe.exe", b"synthetic probe")
        speaking = import_speech(base, data, package, binary, media)
        singing, model, configuration = import_rvc(base, data, binary, media)
        write_creation_tools(base, data, media)

        verify_http(binary, data, fish_id, owner_id, persistent, speaking, singing, install=True)
        verify_http(binary, data, fish_id, owner_id, persistent, speaking, singing, install=False)

        assert source_hashes == {path: sha256(path) for path in source_hashes}
        assert sha256(model) == json.loads(configuration.read_text(encoding="utf-8"))["rvc_model_sha256"]

    print(json.dumps({
        "identity_and_model": True,
        "project_bytes": True,
        "speech_and_rvc_ids": True,
        "default_workspace_creation_tools": True,
        "four_tools_enabled_after_restart": True,
        "source_hashes_preserved": True,
        "model_calls": 0,
        "wechat_sends": 0,
        "neural_inference": False,
    }, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (AssertionError, FileNotFoundError, json.JSONDecodeError, subprocess.SubprocessError, TimeoutError) as error:
        print(json.dumps({"passed": False, "error": str(error)}, ensure_ascii=False), file=sys.stderr)
        raise SystemExit(1)
