"""MCP stdio bridge for a task-scoped PCM WAV preparation tool."""
from __future__ import annotations

import json
import os
from pathlib import Path
import re
import sys

_RUNTIME_DIR = str(Path(__file__).resolve().parent)
if _RUNTIME_DIR not in sys.path:
    sys.path.insert(0, _RUNTIME_DIR)

from audio_logic import AudioError, MAX_INPUT_BYTES, digest, prepare

FRAME_LIMIT = 2 * 1024 * 1024
TOOLS = [{"name": "audio.prepare", "description": "Prepare a host-authorized PCM WAV as a verified WAV artifact.", "inputSchema": {"type": "object"}}]


def _error(value: object) -> str:
    text = str(value)
    return text if re.fullmatch(r"[a-z_]{1,96}", text) else "audio_invalid_request"


def _inside(root: Path, candidate: Path) -> bool:
    """Check host-minted task paths without resolving AppContainer ancestors.

    Windows can deny reparse-point inspection on an otherwise readable mounted
    task directory. The direct input/output entries are rejected as symlinks;
    after that, lexical containment keeps this bridge aligned with the trusted
    host's task mount boundary.
    """
    try:
        allowed = os.path.normcase(os.path.abspath(root))
        value = os.path.normcase(os.path.abspath(candidate))
        return value == allowed or value.startswith(allowed + os.sep)
    except OSError:
        return False


def _audio_context(meta: object) -> tuple[dict, dict]:
    if not isinstance(meta, dict):
        raise AudioError("audio_context_required")
    station = meta.get("station/audio")
    if not isinstance(station, dict):
        raise AudioError("audio_context_required")
    input_item = station.get("input")
    if not isinstance(input_item, dict) or not isinstance(input_item.get("path"), str):
        raise AudioError("audio_input_context_required")
    output_dir = station.get("output_dir")
    if not isinstance(output_dir, str) or not output_dir:
        raise AudioError("audio_output_context_required")
    return station, input_item


def _input(station: dict, item: dict, arguments: dict) -> Path:
    path = Path(item["path"])
    root_value = station.get("input_root")
    root = Path(root_value) if isinstance(root_value, str) else path.parent
    if not path.is_file() or path.is_symlink() or not _inside(root, path):
        raise AudioError("audio_input_not_authorized")
    if path.stat().st_size > MAX_INPUT_BYTES:
        raise AudioError("audio_input_too_large")
    if path.suffix.lower() != ".wav" or str(item.get("mime_type", "")).lower() not in ("audio/wav", "audio/x-wav", "audio/wave"):
        raise AudioError("audio_pcm_wav_required")
    expected = item.get("sha256")
    if not isinstance(expected, str) or not re.fullmatch(r"[a-f0-9]{64}", expected):
        raise AudioError("audio_input_hash_required")
    if digest(path) != expected:
        raise AudioError("audio_input_hash_mismatch")
    artifact = arguments.get("artifact")
    if artifact is not None:
        if not isinstance(artifact, dict) or artifact.get("artifact_id") != item.get("artifact_id") or artifact.get("sha256") != expected:
            raise AudioError("audio_artifact_not_authorized")
    return path


def _output(station: dict) -> Path:
    output = Path(station["output_dir"])
    root_value = station.get("output_root")
    root = Path(root_value) if isinstance(root_value, str) else output
    try:
        output.mkdir(parents=True, exist_ok=True)
    except OSError:
        raise AudioError("audio_output_unwritable")
    if output.is_symlink() or not _inside(root, output):
        raise AudioError("audio_output_not_authorized")
    return output


def _safe_output(path: Path, output: Path) -> dict:
    if not path.is_file() or path.is_symlink() or not _inside(output, path):
        raise AudioError("audio_output_not_authorized")
    return {"path": str(path), "name": path.name, "mime_type": "audio/wav"}


def call(name: str, arguments: object, meta: object) -> dict:
    if name != "audio.prepare":
        raise AudioError("audio_capability_unavailable")
    if not isinstance(arguments, dict) or set(arguments) - {"artifact", "filename", "start_seconds", "duration_seconds", "channels"}:
        raise AudioError("audio_arguments_invalid")
    station, item = _audio_context(meta)
    source = _input(station, item, arguments)
    output = _output(station)
    product = prepare(source, output, arguments.get("filename"), arguments.get("start_seconds"), arguments.get("duration_seconds"), arguments.get("channels"))
    return {"ok": True, "summary": "audio_prepared", "input_sha256": item["sha256"], "outputs": [_safe_output(Path(product.pop("path")), output) | {"size": product["size"], "sha256": product["sha256"], "verification": product["verification"]}], "coverage": product["coverage"]}


class Wire:
    def __init__(self):
        self.initialized = False

    def read(self):
        line = sys.stdin.readline(FRAME_LIMIT + 1)
        if not line:
            raise EOFError()
        if len(line.encode("utf8")) > FRAME_LIMIT or not line.endswith("\n"):
            raise AudioError("audio_frame_limit")
        return json.loads(line)

    def write(self, response: dict):
        raw = json.dumps(response, ensure_ascii=False, separators=(",", ":"))
        if len(raw.encode("utf8")) > FRAME_LIMIT:
            raw = json.dumps({"jsonrpc": "2.0", "id": response.get("id"), "result": {"ok": False, "error": "audio_response_too_large", "outputs": []}}, separators=(",", ":"))
        sys.stdout.write(raw + "\n")
        sys.stdout.flush()

    def serve(self):
        while True:
            try:
                request = self.read()
            except EOFError:
                return
            except (ValueError, TypeError):
                self.write({"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": "invalid_json"}})
                continue
            if "id" not in request:
                continue
            try:
                if request.get("jsonrpc") != "2.0":
                    raise AudioError("audio_protocol_invalid")
                method = request.get("method")
                if method == "initialize":
                    self.initialized = True
                    result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}}, "serverInfo": {"name": "audio-workshop", "version": "0.1.0"}}
                elif method == "tools/list" and self.initialized:
                    result = {"tools": TOOLS}
                elif method == "ping" and self.initialized:
                    result = {}
                elif method == "tools/call" and self.initialized:
                    params = request.get("params", {})
                    result = call(params.get("name", ""), params.get("arguments"), params.get("_meta", {}))
                else:
                    raise AudioError("audio_method_unavailable")
            except (AudioError, ValueError, TypeError, KeyError) as exc:
                result = {"ok": False, "error": _error(exc), "outputs": []}
            self.write({"jsonrpc": "2.0", "id": request["id"], "result": result})


if __name__ == "__main__":
    sys.stdin.reconfigure(encoding="utf8")
    sys.stdout.reconfigure(encoding="utf8")
    Wire().serve()
