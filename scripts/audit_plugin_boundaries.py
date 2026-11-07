"""Read-only inventory and bounded static checks for the lightweight core.

Names are inventory only. This is not proof of all runtime/business behavior.
"""
import argparse
import hashlib
import json
import re
import subprocess
from collections import defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
BUILD_ROOTS = ("./cmd/wechat-admin", "./cmd/wechat-bot")
MODULE = "wechat-bot/"
PLUGIN_IMPLEMENTATION_PREFIX = MODULE + "plugins/"
DIRECT_DEPS = {MODULE + "internal/bailian": "speech", MODULE + "internal/mobile": "mobile-control", MODULE + "internal/channels": "share-reader", MODULE + "internal/dev": "development-only"}
RETAINED_AUTHORITIES = {MODULE + "internal/agenthost": "Core OS isolation, authorized filesystem/process execution and cleanup authority; project interpretation belongs to the installed package."}
SERVICE_MARKERS = {
    "speech": (r"bailian\.New\(", r"speech_synthesize", r"voice_synthesis", r"gpt.?sovits", r"voice_library_"),
    "music": (r"(?<![A-Za-z])YuE(?![A-Za-z])", r"(?<![A-Za-z])RVC(?![A-Za-z])", r"music_(?:local|rvc|service|reference|sampling|resume)"),
    "media-understanding": (r"mediaUnderstanding", r"sampleVideo\(", r"video_inspect", r"transcribeAudio\("),
    "share-reader": (r"channels\.ResolveObject", r"ReadPublicShare", r"resolveReceivedChannels", r"ChannelsShareURL"),
    "mobile-control": (r"\.mobile\b", r"mobileRoutes\(", r"mobile\.Task", r"mobile\.Settings"),
    "project-development": (r"agenthost\.", r"workspaceRoutes\(", r"projectTools\(", r"server_(?:exec|process|windows_process)"),
    "web-search": (r"mcptools\.(?:Search|Read|Fetch)", r"WebFetchEnabled", r"web_search|web_fetch"),
    "visual-generation": (r"comfy", r"visual_generate", r"image_generation"),
    "external-tools": (r"external__tool", r"external[_-]tool", r"mcp\."),
}
# Reviewed semantic contracts, not a source/commit baseline. A new ID needs
# explicit authority/readiness review. False means a future capability stays hidden.
CAPABILITIES = {
    "audio-workshop": {"audio.prepare": (True, {"artifact.read", "artifact.write"})},
    "comfyui": {"comfyui.workflows": (True, {"gpu"}), "comfyui.run": (True, {"artifact.write", "gpu"}), "comfyui.job": (True, {"artifact.write", "gpu"})},
    "document-workshop": {"document.inspect": (True, {"artifact.read"}), "document.reconstruct": (True, {"artifact.read", "artifact.write"}), "document.export_pdf": (True, {"artifact.read", "artifact.write"}), "document.verify": (True, {"artifact.read", "artifact.write"})},
    "external-tools": {"external.http_request": (True, {"network"}), "external.job_request": (True, {"network"}), "external.mcp_request": (True, {"network"}), "external.poll_request": (True, {"network"}), "external.discover": (False, {"network"}), "external.call": (False, {"network"})},
    "media-understanding": {"media.observe": (True, {"artifact.read", "model.invoke"}), "media.transcribe": (True, {"artifact.read", "model.invoke"}), "media.listen": (True, {"artifact.read", "model.invoke"})},
    "mobile-control": {"device.plan": (True, {"model.invoke"}), "device.list": (True, {"device"}), "device.observe": (True, {"device"}), "device.act": (True, {"device"}), "device.record": (True, {"device"}), "device.read_artifact": (True, {"device"}), "device.workflow": (True, {"device", "model.invoke"})},
    "music": {"music.generate": (True, {"model.invoke"}), "music.bridge_request": (True, {"network"}), "music.service_control": (True, {"network"}), "music.prepare": (True, {"network"}), "music.cover": (False, set()), "music.train": (False, set())},
    "project-development": {"project.workspace_read": (True, {"workspace.read"}), "project.workspace_write": (True, {"workspace.write"}), "project.workspace_execute": (True, {"process.execute"})},
    "rvc": {"rvc.convert": (True, {"artifact.read", "artifact.write", "gpu"})},
    "station-speech": {"station-speech.voices": (True, {"artifact.write", "gpu"}), "station-speech.synthesize": (True, {"artifact.write", "gpu"})},
    "yue": {"yue.generate": (True, {"artifact.read", "artifact.write", "gpu"}), "yue.job": (True, {"artifact.read", "artifact.write", "gpu"})},
    "share-reader": {"share.read": (False, set()), "share.resolve_public": (True, {"network"}), "share.resolve_channels": (True, {"network"})},
    "speech": {"speech.synthesize": (True, {"model.invoke"}), "speech.synthesize_named": (True, {"artifact.read", "model.invoke"}), "speech.voices": (False, set()), "speech.train": (False, set())},
    "visual-generation": {"visual.generate": (False, {"network", "artifact.write"})},
    "web-search": {"web.search": (True, {"network"}), "web.read": (True, {"network"}), "web.discover": (True, {"network"})},
}
PACKAGE_PERMISSIONS = {key: set().union(*(rule[1] for rule in rules.values())) for key, rules in CAPABILITIES.items()}
# Documented media artifact-output role; each callback's requirements remain bounded.
PACKAGE_PERMISSIONS["media-understanding"].add("artifact.write")
REQUIRED_FILES = {"capabilities.json", "config.schema.json", "ui.schema.json", "README.md", "PERMISSIONS.md", "MIGRATION.md", "CHANGELOG.md"}
LEGACY_CALLS = {MODULE + "internal/mcptools": {"ReadPublicShare", "DownloadPublicVideoObservation"}, MODULE + "internal/channels": {"ResolveObject"}, MODULE + "internal/bailian": {"New", "NewClient"}}
OPTIONAL_PROVIDER_ROUTE = re.compile(r"SpeechSynthesizer|/audio/(?:transcriptions|asr)|/services/audio/(?:asr|music)|/images/generations|(?:comfyui?|comfy)[^\s]*?/prompt", re.I)


def category(path):
    name = path.name
    if name.endswith("_tools.go") or name in {"agent_tools.go", "service_plugin_tools.go"}: return "tool"
    if name.endswith("_settings.go") or name in {"manager.go", "storage.go", "mcp.go"}: return "configuration"
    if "workspace" in name or "runtime" in name or "process" in name: return "runtime"
    if name in {"agent_workspace.go", "capabilities.go"}: return "prompt"
    if name in {"server.go", "agent_control.go", "mobile.go"}: return "route"
    return "implementation"


def source_hits(root=ROOT):
    result = defaultdict(list)
    patterns = {key: [re.compile(p, re.I) for p in values] for key, values in SERVICE_MARKERS.items()}
    for folder in (root / "internal", root / "cmd"):
        for path in sorted(folder.rglob("*.go")):
            if path.name.endswith("_test.go"): continue
            text = path.read_text(encoding="utf-8", errors="replace")
            for plugin, expressions in patterns.items():
                markers = [p.pattern for p in expressions if p.search(text)]
                if markers: result[plugin].append({"path": path.relative_to(root).as_posix(), "kind": category(path), "markers": markers, "inventory_only": True})
    return dict(sorted(result.items()))


def static_plugin_implementation_dependencies(dependencies):
    return [{"package": package, "target_plugin": package[len(PLUGIN_IMPLEMENTATION_PREFIX):].split("/")[0]} for package in sorted(set(dependencies)) if package.startswith(PLUGIN_IMPLEMENTATION_PREFIX)]


def go_lex(text):
    """Mask Go comments/literals, retaining offsets and string tokens."""
    code, literals, index = list(text), [], 0
    while index < len(text):
        start = index
        if text.startswith("//", index):
            index = text.find("\n", index)
            if index < 0: index = len(text)
        elif text.startswith("/*", index):
            end = text.find("*/", index + 2); index = len(text) if end < 0 else end + 2
        elif text[index] in ('"', "'", '`'):
            quote = text[index]; index += 1
            while index < len(text):
                if quote != '`' and text[index] == "\\": index += 2; continue
                if text[index] == quote: index += 1; break
                index += 1
            if quote != "'":
                token = text[start:index]
                try: value = token[1:-1] if quote == '`' else json.loads(token)
                except (ValueError, TypeError): value = token[1:-1]
                literals.append((start, index, value))
        else: index += 1; continue
        for offset in range(start, min(index, len(text))):
            if text[offset] != "\n": code[offset] = " "
    return "".join(code), literals


def compiled_source_violations(root, packages):
    violations = []
    for package in packages:
        if not package.get("ImportPath", "").startswith(MODULE): continue
        for name in sorted(set(package.get("GoFiles", []) + package.get("CgoFiles", []))):
            path = Path(package.get("Dir", root)) / name
            if path.name.endswith("_test.go"): continue
            try: relative = path.resolve().relative_to(root.resolve()).as_posix()
            except ValueError: continue
            if not path.is_file(): violations.append({"kind": "compiled_source_unavailable", "path": relative}); continue
            text = path.read_text(encoding="utf-8")
            code, literals = go_lex(text); aliases = {}
            for start, _, value in literals:
                line = code[code.rfind("\n", 0, start) + 1:start]
                match = re.fullmatch(r"\s*(?:import\s+)?(?:(\w+|\.)\s+)?", line)
                if match and (value in LEGACY_CALLS or value == "net/http"): aliases[value] = match.group(1) or value.rsplit("/", 1)[-1]
            def add(kind, offset, symbol):
                violations.append({"kind": kind, "path": relative, "line": text.count("\n", 0, offset) + 1, "symbol": symbol})
            for module, members in LEGACY_CALLS.items():
                alias = aliases.get(module)
                if not alias or alias == "_": continue
                prefix = r"(?<![\w.])" if alias == "." else r"\b" + re.escape(alias) + r"\s*\.\s*"
                for member in sorted(members):
                    for match in re.finditer(prefix + re.escape(member) + r"\s*\(", code): add("legacy_optional_execution_call", match.start(), member)
            for match in re.finditer(r"\b(?:\w+\s*\.\s*)?runtime\s*\.\s*Call\s*\(", code): add("legacy_external_runtime_call", match.start(), "runtime.Call")
            http = aliases.get("net/http")
            if not http or http in ("_", "."): continue
            provider_literals = [(start, end, value) for start, end, value in literals if OPTIONAL_PROVIDER_ROUTE.search(value)]
            bindings = set()
            for start, _, _ in provider_literals:
                match = re.search(r"\b([A-Za-z_]\w*)\s*(?::=|=)\s*$", code[max(0, start - 160):start])
                if match: bindings.add(match.group(1))
            receivers = [re.escape(http), re.escape(http) + r"\s*\.\s*DefaultClient"]
            for match in re.finditer(r"\b(\w+)\s*(?::=|=)\s*&?\s*" + re.escape(http) + r"\s*\.\s*Client\b", code): receivers.append(re.escape(match.group(1)))
            expression = r"\b(?:" + "|".join(receivers) + r")\s*\.\s*(?:Post|Get|NewRequest|NewRequestWithContext)\s*\("
            for match in re.finditer(expression, code):
                depth, end = 1, match.end()
                while end < len(code) and depth:
                    if code[end] == '(': depth += 1
                    elif code[end] == ')': depth -= 1
                    end += 1
                direct = any(match.end() <= start < end for start, _, _ in provider_literals)
                assigned = any(re.search(r"\b" + re.escape(name) + r"\b", code[match.end():end]) for name in bindings)
                if direct or assigned: add("inline_optional_provider_http", match.start(), "HTTP optional-provider route")
    return sorted(violations, key=lambda row: (row["path"], row.get("line", 0), row["kind"]))


def safe_relative(value):
    return isinstance(value, str) and bool(value) and ":" not in value and "\\" not in value and not value.startswith("/") and all(part not in ("", ".", "..") for part in value.split("/"))


def permission_set(value):
    if not isinstance(value, list) or any(not isinstance(item, str) for item in value): return None
    return set(value) if len(set(value)) == len(value) else None


def strict_json(path):
    def pairs(rows):
        out = {}
        for key, value in rows:
            if key in out: raise ValueError("duplicate JSON key")
            out[key] = value
        return out
    return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=pairs)


def audit_catalog(root=ROOT):
    inventory, violations = [], []
    for directory in sorted((root / "plugins").iterdir()) if (root / "plugins").is_dir() else []:
        if directory.is_dir() and not (directory / "plugin.json").is_file():
            violations.append({"kind": "plugin_contract", "path": directory.relative_to(root).as_posix(), "reason": "package_manifest_missing"})
    for manifest in sorted((root / "plugins").glob("*/plugin.json")):
        directory, relative = manifest.parent, manifest.relative_to(root).as_posix()
        def fail(reason, capability=None):
            item = {"kind": "plugin_contract", "path": relative, "reason": reason}
            if capability is not None: item["capability"] = capability
            violations.append(item)
        try:
            package = strict_json(manifest); doc = strict_json(directory / "capabilities.json")
            identity, status, caps = package["id"], package["status"], doc["capabilities"]
            if not isinstance(identity, str) or not isinstance(caps, list) or doc.get("version") != 1: raise ValueError("invalid capability document")
        except (OSError, ValueError, KeyError, TypeError): fail("invalid_package_or_capability_document"); continue
        runtime = package.get("runtime", {}); permissions = package.get("permissions", []); files = package.get("files", {})
        if not isinstance(runtime, dict): fail("invalid_runtime"); runtime = {}
        declared_permissions = permission_set(permissions)
        rows = [{"id": c.get("id"), "implementation": c.get("implementation"), "discoverable": c.get("discoverable") is True} for c in caps if isinstance(c, dict)]
        inventory.append({"id": identity, "status": status, "runtime": runtime.get("entrypoint", ""), "capabilities": rows})
        if identity != directory.name or identity not in CAPABILITIES: fail("unreviewed_package_identity"); continue
        if package.get("schema_version") != 1 or package.get("host_protocol") != 1: fail("unsupported_package_protocol")
        if status not in ("ready", "template", "migration-pending"): fail("invalid_package_status")
        if declared_permissions is None or not declared_permissions <= PACKAGE_PERMISSIONS[identity]: fail("package_permission_outside_reviewed_role")
        if not isinstance(files, dict) or not REQUIRED_FILES <= set(files): fail("required_package_files_missing"); files = files if isinstance(files, dict) else {}
        actual = set()
        for path in directory.rglob("*"):
            if path.is_symlink(): fail("package_link_forbidden"); continue
            if not path.is_file() or path == manifest: continue
            name = path.relative_to(directory).as_posix(); actual.add(name)
            if name not in files: fail("unindexed_package_file"); continue
            with path.open("rb") as stream: digest = hashlib.file_digest(stream, "sha256").hexdigest()
            if digest != files[name]: fail("package_file_digest_mismatch")
        if any(not safe_relative(name) for name in files) or set(files) != actual: fail("package_file_index_invalid")
        ready, entry = status == "ready", runtime.get("entrypoint", "")
        if ready:
            if runtime.get("transport") not in ("mcp-stdio", "mcp-http", "http-adapter") or not safe_relative(entry) or entry not in files or not (directory / entry).is_file() or (directory / entry).stat().st_size == 0: fail("ready_runtime_unavailable")
        elif runtime.get("transport") != "unbound" or entry or package.get("http_routes"): fail("template_has_executable_binding")
        seen = set()
        for cap in caps:
            if not isinstance(cap, dict): fail("invalid_capability"); continue
            name = cap.get("id")
            if not isinstance(name, str): fail("invalid_capability_id"); continue
            if name in seen: fail("duplicate_capability", name)
            seen.add(name); rule = CAPABILITIES[identity].get(name)
            if rule is None: fail("unreviewed_capability", name); continue
            cap_permissions = permission_set(cap.get("permissions", []))
            if cap_permissions is None or cap_permissions != rule[1] or not cap_permissions <= (declared_permissions or set()): fail("capability_permission_mismatch", name)
            implementation = cap.get("implementation")
            if implementation not in ("ready", "migration-pending", "template"): fail("invalid_capability_status", name)
            if implementation == "ready" and (not ready or not rule[0]): fail("unreviewed_or_unbound_ready_capability", name)
            if implementation != "ready" and cap.get("discoverable") is not False: fail("template_capability_discoverable", name)
            if not isinstance(cap.get("discoverable"), bool): fail("invalid_discoverability", name)
            if any(not isinstance(cap.get(key), dict) or cap[key].get("type") != "object" for key in ("input_schema", "output_schema")): fail("invalid_capability_schema", name)
        if ready:
            ready_ids = {c.get("id") for c in caps if isinstance(c, dict) and isinstance(c.get("id"), str) and c.get("implementation") == "ready"}
            for name, rule in CAPABILITIES[identity].items():
                if rule[0] and name not in ready_ids: fail("required_ready_capability_missing", name)
            if not ready_ids: fail("ready_package_without_ready_capability")
    for identity in sorted(CAPABILITIES.keys() - {item["id"] for item in inventory}):
        violations.append({"kind": "plugin_contract", "path": "plugins/" + identity, "reason": "required_catalog_plugin_missing"})
    if not inventory: violations.append({"kind": "plugin_contract", "reason": "plugin_catalog_empty"})
    return inventory, violations


def selected_go_packages(root=ROOT):
    try:
        result = subprocess.run(["go", "list", "-deps", "-json", *BUILD_ROOTS], cwd=root, text=True, capture_output=True, encoding="utf-8", check=False)
    except OSError:
        return [], [{"kind": "build_graph_unavailable", "reason": "go_list_unavailable"}]
    if result.returncode: return [], [{"kind": "build_graph_unavailable", "reason": "go_list_failed"}]
    decoder, packages, offset = json.JSONDecoder(), [], 0
    try:
        while offset < len(result.stdout):
            if result.stdout[offset].isspace(): offset += 1; continue
            value, offset = decoder.raw_decode(result.stdout, offset)
            if value.get("Error") or value.get("Incomplete"): raise ValueError("incomplete Go graph")
            packages.append(value)
    except (ValueError, TypeError, AttributeError): return [], [{"kind": "build_graph_unavailable", "reason": "invalid_go_list_output"}]
    expected = {MODULE + item.removeprefix("./") for item in BUILD_ROOTS}
    if not expected <= {item.get("ImportPath") for item in packages}:
        return [], [{"kind": "build_graph_unavailable", "reason": "build_root_missing"}]
    return packages, []


def build_report(root=ROOT, packages=None):
    packages, graph_errors = selected_go_packages(root) if packages is None else (packages, [])
    dependencies = {item["ImportPath"] for item in packages}; remaining = []
    for path in sorted(dependencies):
        target = next((plugin for prefix, plugin in DIRECT_DEPS.items() if path == prefix or path.startswith(prefix + "/")), None)
        if target or path.startswith(MODULE) and "dev" in path[len(MODULE):].split("/"): remaining.append({"package": path, "target_plugin": target or "development-only"})
    static = static_plugin_implementation_dependencies(dependencies)
    inventory, catalog_errors = audit_catalog(root); execution = compiled_source_violations(root, packages); hits = source_hits(root)
    blockers = graph_errors + [{"kind": "legacy_optional_dependency", **row} for row in remaining] + [{"kind": "static_plugin_implementation_dependency", **row} for row in static] + execution + catalog_errors
    return {
        "light_core_ready": not blockers, "static_boundary_passed": not blockers, "audit_only": True,
        "build_roots": list(BUILD_ROOTS), "gate_blockers": blockers,
        "direct_optional_dependencies": remaining, "static_plugin_implementation_dependencies": static,
        "source_execution_violations": execution,
        "retained_dependency_authorities": [{"package": name, "reason": reason} for name, reason in RETAINED_AUTHORITIES.items() if name in dependencies],
        "plugins": {item["id"]: {**item, "core_references": hits.get(item["id"], []), "available_capabilities": sum(c["implementation"] == "ready" and c["discoverable"] and item["status"] == "ready" for c in item["capabilities"])} for item in inventory},
        "totals": {"plugins": len(inventory), "ready_plugins": sum(item["status"] == "ready" for item in inventory), "core_optional_source_files": sum(len(rows) for rows in hits.values()), "core_optional_dependency_packages": len(remaining), "core_static_plugin_implementation_packages": len(static), "source_execution_violations": len(execution), "gate_blockers": len(blockers)},
        "core_boundary": {"retained": ["native_wechat_login_and_send", "account_and_auth", "memory_and_context", "asset_identity_and_delivery", "media_encoding_and_staging", "plugin_host", "os_isolation_and_execution_authority"], "excluded_from_scan": ["tests", "account_data", "models", "runtimes", "pending_cleanup"], "inventory_names_are_not_gate_failures": True},
        "limitations": ["Selected Go build graph only; run per supported GOOS/GOARCH and build tags.", "Known source execution regressions only; not an exhaustive call-graph or semantic proof.", "Package files, declared readiness and reviewed permissions are checked without executing plugins.", "Does not verify OS sandbox behavior, model decisions, provider execution, WeChat acceptance or playback."],
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--require-light", action="store_true"); parser.add_argument("--compact", action="store_true")
    args = parser.parse_args(); report = build_report()
    if args.compact:
        for item in report["plugins"].values(): item["core_references"] = [{"path": row["path"], "kind": row["kind"], "inventory_only": True} for row in item["core_references"]]
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return int(args.require_light and not report["light_core_ready"])


if __name__ == "__main__":
    raise SystemExit(main())
