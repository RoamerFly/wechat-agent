"""MCP bridge for document-workshop.

The host owns artifact resolution and passes an ephemeral, task-scoped document
envelope.  Model arguments carry an artifact id, never a local path.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import re
import sys

# The managed interpreter deliberately starts with -I, which removes the
# entrypoint directory from sys.path together with ambient PYTHONPATH.  This
# package needs its manifest-bound sibling modules only; add precisely this
# verified runtime directory before importing them.  No request or plugin
# setting can select another path.
_RUNTIME_DIR = str(Path(__file__).resolve().parent)
if _RUNTIME_DIR not in sys.path:
    sys.path.insert(0, _RUNTIME_DIR)

from document_logic import DocumentError, basic_verify, inspect, reconstruct, revise_title

FRAME_LIMIT = 4 * 1024 * 1024
TOOLS = [
    {"name": "document.inspect", "description": "Inspect a host-authorized document artifact.", "inputSchema": {"type": "object"}},
    {"name": "document.reconstruct", "description": "Create an editable DOCX from verified layout.", "inputSchema": {"type": "object"}},
    {"name": "document.revise_title", "description": "Resize the main title of a verified editable DOCX.", "inputSchema": {"type": "object"}},
    {"name": "document.export_pdf", "description": "Export a host-authorized DOCX to PDF.", "inputSchema": {"type": "object"}},
    {"name": "document.verify", "description": "Verify a host-authorized DOCX or PDF.", "inputSchema": {"type": "object"}},
]


def _error(value: object) -> str:
    text = str(value)
    return text if re.fullmatch(r"[a-z_]{1,96}", text) else "document_invalid_request"


def _inside(root: Path, candidate: Path) -> bool:
    try:
        candidate.resolve(strict=True).relative_to(root.resolve(strict=True))
        return True
    except (OSError, ValueError):
        return False


def _context(meta: dict) -> tuple[dict, dict]:
    context = meta.get("wechat/context", {})
    document = meta.get("wechat/document", {})
    if not isinstance(context, dict) or not all(context.get(k) for k in ("account", "thread", "requester", "purpose", "request_key")):
        raise DocumentError("host_context_required")
    if not isinstance(document, dict) or not isinstance(document.get("inputs"), list):
        raise DocumentError("document_context_required")
    return context, document


def _artifact_id(arguments: dict) -> str:
    candidate = arguments.get("artifact_id", arguments.get("artifact"))
    if isinstance(candidate, dict):
        candidate = candidate.get("artifact_id")
    if not isinstance(candidate, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}", candidate):
        raise DocumentError("document_artifact_required")
    return candidate


def _input(document: dict, arguments: dict) -> tuple[Path, dict]:
    artifact_id = _artifact_id(arguments)
    found = next((row for row in document["inputs"] if isinstance(row, dict) and row.get("artifact_id") == artifact_id), None)
    if not found or not isinstance(found.get("path"), str):
        raise DocumentError("document_artifact_not_authorized")
    path = Path(found["path"])
    root = Path(document["input_root"]) if isinstance(document.get("input_root"), str) else path.parent
    if not path.is_file():
        raise DocumentError("document_artifact_input_missing")
    if path.is_symlink():
        raise DocumentError("document_artifact_not_authorized")
    # Input roots are host-minted and mounted read-only. Use the same lexical
    # containment form as output validation: strict resolve may be denied for
    # an AppContainer-mounted ancestor despite the regular file being readable.
    try:
        allowed = os.path.normcase(os.path.abspath(root))
        candidate = os.path.normcase(os.path.abspath(path))
    except OSError:
        raise DocumentError("document_artifact_not_authorized")
    if candidate == allowed or not candidate.startswith(allowed + os.sep):
        raise DocumentError("document_artifact_not_authorized")
    if path.stat().st_size > 128 * 1024 * 1024:
        raise DocumentError("document_too_large")
    return path, found


def _images(document: dict, arguments: dict) -> list[Path]:
    requested = arguments.get("images", [])
    if not isinstance(requested, list) or len(requested) > 20:
        raise DocumentError("document_images_invalid")
    paths = []
    for value in requested:
        # Reuse the exact artifact lookup with a narrow synthetic argument;
        # no image member can supply a filesystem path.
        path, record = _input(document, {"artifact": value})
        if path.suffix.lower() not in (".png", ".jpg", ".jpeg") or not str(record.get("mime_type", "")).startswith("image/"):
            raise DocumentError("document_image_not_supported")
        paths.append(path)
    return paths


def _output(document: dict) -> Path:
    value = document.get("output_dir")
    if not isinstance(value, str) or not value:
        raise DocumentError("document_output_context_required")
    path = Path(value)
    root = Path(document["output_root"]) if isinstance(document.get("output_root"), str) else path
    try:
        path.mkdir(parents=True, exist_ok=True)
    except OSError:
        raise DocumentError("document_output_unwritable")
    if path.is_symlink():
        raise DocumentError("document_output_symlink_denied")
    # On AppContainer Windows, Path.resolve(strict=True) can reject a mounted
    # directory even after mkdir succeeds because ancestor reparse inspection
    # is denied. Both values are host-minted metadata; use a lexical boundary
    # check here after rejecting the output itself as a symlink. The host
    # independently rejects reparse points while minting task directories.
    try:
        allowed = os.path.normcase(os.path.abspath(root))
        candidate = os.path.normcase(os.path.abspath(path))
    except OSError:
        raise DocumentError("document_output_not_authorized")
    if candidate != allowed and not candidate.startswith(allowed + os.sep):
        raise DocumentError("document_output_not_authorized")
    return path


def _safe_output(path: Path, output: Path, role: str, mime_type: str) -> dict:
    if not path.is_file() or path.is_symlink():
        raise DocumentError("document_output_not_authorized")
    try:
        root = os.path.normcase(os.path.abspath(output))
        candidate = os.path.normcase(os.path.abspath(path))
    except OSError:
        raise DocumentError("document_output_not_authorized")
    if candidate == root or not candidate.startswith(root + os.sep):
        raise DocumentError("document_output_not_authorized")
    # The path is for the host artifact registrar; model-facing host code must
    # replace it with a materialized artifact id before placing it in context.
    return {"path": str(path), "role": role, "mime_type": mime_type, "display_name": path.name}


def _runtime(action: str, request: dict) -> dict:
    # The MCP process already runs inside the plugin sandbox. In particular,
    # do not spawn the Windows console interpreter just to call another module.
    from document_runtime import dispatch
    return dispatch({"action": action, **request})


def call(name: str, arguments: dict, meta: dict) -> dict:
    if not isinstance(arguments, dict):
        raise DocumentError("document_arguments_invalid")
    _, document = _context(meta)
    if name == "document.inspect":
        path, item = _input(document, arguments)
        mime_type = str(item.get("mime_type", ""))
        if path.suffix.lower() == ".docx":
            return {"ok": True, "summary": "document_inspected", "evidence": inspect(
                path,
                mime_type,
                arguments.get("pages"),
                bool(arguments.get("include_layout")),
                arguments.get("text_offset"),
                arguments.get("text_limit"),
                arguments.get("start_block"),
                arguments.get("block_limit"),
            ), "outputs": []}
        input_root = document.get("input_root") if isinstance(document.get("input_root"), str) else str(path.parent)
        output = _output(document)
        output_root = document.get("output_root") if isinstance(document.get("output_root"), str) else str(output)
        runtime = _runtime("inspect", {"input_root": input_root, "output_root": output_root, "input_path": str(path), "output_dir": str(output), "mime_type": mime_type, "pages": arguments.get("pages", []), "include_layout": bool(arguments.get("include_layout")), "host_ocr": document.get("ocr_observation")})
        if runtime.get("ok") is not True:
            raise DocumentError(runtime.get("code", "document_runtime_inspection_failed"))
        return {"ok": True, "summary": "document_inspected", "evidence": runtime, "outputs": []}
    if name == "document.reconstruct":
        if set(arguments) - {"layout", "title", "title_font_size_pt", "images", "filename"}:
            raise DocumentError("document_arguments_invalid")
        output = _output(document)
        product = reconstruct(arguments.get("layout"), output, arguments.get("filename", "document.docx"), arguments.get("title", ""), _images(document, arguments), arguments.get("title_font_size_pt"))
        return {"ok": True, "summary": "editable_docx_created", "evidence": {"sha256": product["sha256"], "text_characters": len(product["text"])}, "outputs": [_safe_output(Path(product["path"]), output, "editable_source", product["mime_type"])]}
    if name == "document.revise_title":
        if set(arguments) - {"artifact_id", "font_size_pt", "filename"}:
            raise DocumentError("document_arguments_invalid")
        path, item = _input(document, arguments)
        if path.suffix.lower() != ".docx":
            raise DocumentError("document_title_revision_requires_docx")
        output = _output(document)
        product = revise_title(path, output, arguments.get("filename", "revised.docx"), arguments.get("font_size_pt"))
        evidence = {"sha256": product["sha256"], "input_sha256": item["sha256"], "font_size_pt": product["font_size_pt"]}
        return {"ok": True, "summary": "editable_docx_revised", "evidence": evidence, "outputs": [_safe_output(Path(product["path"]), output, "editable_source", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")]}
    if name == "document.verify":
        path, _ = _input(document, arguments)
        result = basic_verify(path, arguments.get("expected_text", []))
        return {"ok": result["ok"], "summary": "document_verified" if result["ok"] else "document_quality_check_failed", "evidence": result, "outputs": []}
    if name == "document.export_pdf":
        path, _ = _input(document, arguments)
        if path.suffix.lower() != ".docx":
            raise DocumentError("document_export_requires_docx")
        output = _output(document)
        input_root = document.get("input_root") if isinstance(document.get("input_root"), str) else str(path.parent)
        output_root = document.get("output_root") if isinstance(document.get("output_root"), str) else str(output)
        runtime = _runtime("convert", {"input_docx": str(path), "input_root": input_root, "output_dir": str(output), "output_root": output_root, "filename": arguments.get("filename", "document.pdf")})
        if runtime.get("ok") is not True or not isinstance(runtime.get("output_pdf"), str):
            raise DocumentError(runtime.get("code", "document_converter_unavailable"))
        verified = _runtime("verify", {"input_pdf": runtime["output_pdf"], "output_root": output_root})
        if verified.get("ok") is not True:
            raise DocumentError(verified.get("code", "pdf_verification_failed"))
        return {"ok": True, "summary": "pdf_exported", "evidence": {"export": {key: runtime.get(key) for key in ("backend", "supported_layout", "bytes", "sha256")}, "verification": verified}, "outputs": [_safe_output(Path(runtime["output_pdf"]), output, "delivery_pdf", "application/pdf")]}
    raise DocumentError("document_capability_unavailable")


class Wire:
    def __init__(self):
        self.initialized = False

    def read(self):
        line = sys.stdin.readline(FRAME_LIMIT + 1)
        if not line:
            raise EOFError()
        if len(line.encode("utf8")) > FRAME_LIMIT or not line.endswith("\n"):
            raise DocumentError("document_frame_limit")
        return json.loads(line)

    def write(self, response):
        raw = json.dumps(response, ensure_ascii=False, separators=(",", ":"))
        if len(raw.encode("utf8")) > FRAME_LIMIT:
            raw = json.dumps({"jsonrpc": "2.0", "id": response.get("id"), "result": {"ok": False, "error": "document_response_too_large"}}, separators=(",", ":"))
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
                    raise DocumentError("document_protocol_invalid")
                method = request.get("method")
                if method == "initialize":
                    self.initialized = True
                    result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}}, "serverInfo": {"name": "document-workshop", "version": "0.1.0"}}
                elif method == "tools/list" and self.initialized:
                    result = {"tools": TOOLS}
                elif method == "ping" and self.initialized:
                    result = {}
                elif method == "tools/call" and self.initialized:
                    params = request.get("params", {})
                    result = call(params.get("name", ""), params.get("arguments"), params.get("_meta", {}))
                else:
                    raise DocumentError("document_method_unavailable")
            except (DocumentError, ValueError, TypeError, KeyError) as exc:
                result = {"ok": False, "error": _error(exc), "outputs": []}
            self.write({"jsonrpc": "2.0", "id": request["id"], "result": result})


if __name__ == "__main__":
    sys.stdin.reconfigure(encoding="utf8")
    sys.stdout.reconfigure(encoding="utf8")
    Wire().serve()
