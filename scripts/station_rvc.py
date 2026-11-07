"""Narrow, local-only bindings for the Station RVC conversion endpoint.

This module deliberately owns no account configuration and starts no process.
The bridge supplies its already-isolated public/private mounts, then uses these
helpers to bind a fish identity, validate an explicitly registered RVC profile,
and serve only a verified conversion result.
"""

import hashlib
import json
import math
import os
from pathlib import Path
import re
import struct

from managed_music_runner import canonical
import rvc_profiles


HEADER = "X-Agent-Station-Fish"

_FISH = re.compile(r"fish-[0-9a-f]{48}")
_PROFILE = re.compile(r"[A-Za-z0-9_-]{1,128}")
_JOB = re.compile(r"[0-9a-f]{32}")
_SHA256 = re.compile(r"[0-9a-f]{64}")
_MAX_CONFIG = 256 * 1024
_MAX_SIDECAR = 64 * 1024
_MAX_WAV = 128 * 1024 * 1024


def _fail(code):
    raise ValueError(code)


def _absolute(value, code):
    try:
        path = Path(value)
    except (TypeError, ValueError):
        _fail(code)
    if not path.is_absolute():
        _fail(code)
    return Path(os.path.abspath(path))


def _link_free(path, code, *, include_leaf=True):
    """Reject symlinks and Windows junctions without disclosing a path."""
    candidate = Path(path)
    parts = [candidate] if include_leaf else []
    parent = candidate.parent
    while True:
        parts.append(parent)
        if parent == parent.parent:
            break
        parent = parent.parent
    for part in parts:
        try:
            if part.is_symlink() or (hasattr(part, "is_junction") and part.is_junction()):
                _fail(code)
        except OSError:
            _fail(code)


def _directory(value, code):
    path = _absolute(value, code)
    _link_free(path, code)
    if not path.is_dir():
        _fail(code)
    try:
        return canonical(path)
    except (OSError, ValueError):
        _fail(code)


def _regular(value, code, maximum=None):
    path = _absolute(value, code)
    _link_free(path, code)
    try:
        if not path.is_file() or path.is_symlink():
            _fail(code)
        resolved = path.resolve(strict=True)
        if maximum is not None and resolved.stat().st_size > maximum:
            _fail(code)
        return resolved
    except OSError:
        _fail(code)


def _same_path(left, right):
    return os.path.normcase(os.path.abspath(left)) == os.path.normcase(os.path.abspath(right))


def _inside(path, root):
    try:
        return Path(path).is_relative_to(Path(root))
    except ValueError:
        return False


def _digest(path):
    hasher = hashlib.sha256()
    with Path(path).open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            hasher.update(block)
    return hasher.hexdigest()


def _digest_bytes(value):
    return hashlib.sha256(value).hexdigest()


def _create_child(parent, name, code):
    child = parent / name
    _link_free(parent, code)
    try:
        child.mkdir(mode=0o700, exist_ok=True)
    except OSError:
        _fail(code)
    return _directory(child, code)


def bind_scope(fish, public, private, state, token_file):
    """Bind a single fish to exactly one Station public/private mount pair."""
    if not isinstance(fish, str) or not _FISH.fullmatch(fish):
        _fail("station_fish_invalid")
    public = _directory(public, "station_scope_invalid")
    if public.name != "public_workspace":
        _fail("station_scope_invalid")
    workspace = _directory(public.parent, "station_scope_invalid")
    if public.parent != workspace:
        _fail("station_scope_invalid")
    private_root = _directory(workspace / "pri_workspace", "station_scope_invalid")
    scope = hashlib.sha256(fish.encode("ascii")).hexdigest()
    expected_private = private_root / scope
    private = _directory(private, "station_scope_invalid")
    if not _same_path(private, expected_private):
        _fail("station_scope_invalid")

    expected_state = expected_private / "music" / "jobs"
    requested_state = _absolute(state, "station_scope_invalid")
    if not _same_path(requested_state, expected_state):
        _fail("station_scope_invalid")
    music = _create_child(private, "music", "station_scope_invalid")
    state = _create_child(music, "jobs", "station_scope_invalid")

    token = _regular(token_file, "station_token_invalid", 4096)
    # Tokens do not belong in any mount that an RVC configuration or project can
    # traverse. Requiring it outside the common workspace also covers siblings.
    if _inside(token, workspace):
        _fail("station_token_invalid")
    try:
        token_value = token.read_text(encoding="ascii")
    except (OSError, UnicodeDecodeError):
        _fail("station_token_invalid")
    if not re.fullmatch(r"[!-~]{1,4096}", token_value):
        _fail("station_token_invalid")
    return public, private, state, token, scope


def _json_object(path, maximum, code):
    path = _regular(path, code, maximum)
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        _fail(code)
    if not isinstance(value, dict):
        _fail(code)
    return path, value


def _under_file(value, roots, code, suffix=None):
    path = _regular(value, code)
    if suffix is not None and path.suffix.lower() != suffix:
        _fail(code)
    if not any(_inside(path, root) for root in roots):
        _fail(code)
    return path


def _under_directory(value, roots, code):
    path = _directory(value, code)
    if not any(_inside(path, root) for root in roots):
        _fail(code)
    return path


def _rvc_assets(project, code):
    """Check the two upstream inference assets used by the registered project.

    This is deliberately not an HDemucs readiness check: separation is an
    optional later stage and its downloader must remain offline in Station
    mode.  The converter itself loads HuBERT and RMVPE from this fixed project
    layout, so accepting a profile without them would advertise a false-ready
    RVC service.
    """
    assets = _under_directory(project / "assets", [project], code)
    # Current upstream and an older deployed RVC layout use different HuBERT
    # names. Both are fixed inside this one registered project, never searched.
    modern = assets / "hubert_base" / "pytorch_model.bin"
    legacy = assets / "hubert" / "hubert_base.pt"
    if modern.exists():
        _under_file(modern, [assets], code)
    elif legacy.exists():
        _under_file(legacy, [assets], code)
    else:
        _fail(code)
    _under_file(assets / "rmvpe" / "rmvpe.pt", [assets], code)
    if (project / "infer" / "vc" / "modules.py").exists():
        _under_file(project / "infer" / "vc" / "modules.py", [project], code)
    else:
        _under_file(project / "infer" / "modules" / "vc" / "modules.py", [project], code)


def _hash_field(value, code):
    if not isinstance(value, str) or not _SHA256.fullmatch(value):
        _fail(code)
    return value


def checked_profile(public, private, body, profile_reader):
    """Resolve one registered voice profile and return safe, pinned metadata.

    All path checks occur before ``profile_reader``. This matters because some
    readers initialize a runtime while parsing their configuration.
    """
    public = _directory(public, "station_profile_invalid")
    private = _directory(private, "station_profile_invalid")
    if not isinstance(body, dict):
        _fail("station_profile_invalid")
    identity = body.get("voice_profile_id")
    if not isinstance(identity, str) or not _PROFILE.fullmatch(identity):
        _fail("station_profile_invalid")
    # ``resolve`` reads this registration. Establish that its selected
    # directory is a real child before an untrusted id selects a manifest.
    manifest_root = (private / "voice-library" / "manifests"
                     if identity.startswith("private-") else public / "manifests")
    _directory(manifest_root, "station_profile_invalid")
    try:
        config_name = rvc_profiles.resolve(str(public), None, body, str(private))
    except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError):
        _fail("station_profile_invalid")
    if not config_name:
        _fail("station_profile_invalid")

    chosen_root = (_directory(private / "voice-library", "station_profile_invalid")
                   if identity.startswith("private-") else
                   _directory(public / "models" / "voice-library", "station_profile_invalid"))
    config = _under_file(config_name, [chosen_root], "station_profile_invalid", ".json")
    config, item = _json_object(config, _MAX_CONFIG, "station_profile_invalid")
    profile_label = item.get("profile")
    if (not isinstance(profile_label, str) or not _PROFILE.fullmatch(profile_label)
            or item.get("engine") != "rvc" or item.get("enabled") is not True):
        _fail("station_profile_invalid")

    runtimes = _directory(public / "runtimes", "station_profile_invalid")
    projects = _directory(public / "projects", "station_profile_invalid")
    python = _under_file(item.get("python"), [runtimes], "station_profile_invalid", ".exe")
    rvc_python = _under_file(item.get("rvc_python"), [runtimes], "station_profile_invalid", ".exe")
    runtime = _directory(item.get("runtime"), "station_profile_invalid")
    if not _inside(runtime, runtimes):
        # The deployed Windows registration has one narrow legacy exception;
        # do not turn it into permission for arbitrary public model folders.
        separator = _directory(public / "models" / "separator", "station_profile_invalid")
        if not _same_path(runtime, separator):
            _fail("station_profile_invalid")
    project = _under_directory(item.get("rvc_project"), [projects], "station_profile_invalid")
    _rvc_assets(project, "station_profile_invalid")
    model = _under_file(item.get("rvc_model"), [chosen_root], "station_profile_invalid")
    index = _under_file(item.get("rvc_index"), [chosen_root], "station_profile_invalid")
    model_hash = _hash_field(item.get("rvc_model_sha256"), "station_profile_invalid")
    index_hash = _hash_field(item.get("rvc_index_sha256"), "station_profile_invalid")
    if _digest(model) != model_hash or _digest(index) != index_hash:
        _fail("station_profile_invalid")
    device = item.get("rvc_device")
    if device not in ("auto", "cpu", "cuda:0"):
        _fail("station_profile_invalid")
    config_hash = _digest(config)
    # Keep locals above explicit: they make the role-bound paths auditable and
    # intentionally prevent a reader from being the first path validator.
    del python, rvc_python, runtime, project
    try:
        read = profile_reader(str(config), config_hash)
    except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError):
        _fail("station_profile_invalid")
    if not isinstance(read, dict) or read.get("profile") != profile_label or str(read.get("engine", "")).lower() != "rvc":
        _fail("station_profile_invalid")
    reported = read.get("config_sha256")
    if reported is not None and reported != config_hash:
        _fail("station_profile_invalid")
    return str(config), {
        "profile": profile_label,
        "engine": "rvc",
        "config_sha256": config_hash,
        "model_sha256": model_hash,
        "index_sha256": index_hash,
        "requested_device": device,
    }


def _job_id(jid):
    if not isinstance(jid, str) or not _JOB.fullmatch(jid):
        _fail("station_rvc_output_invalid")


def _wav_evidence(path):
    """Parse a PCM WAV strictly enough to reject truncated/forged results."""
    size = path.stat().st_size
    if size < 44 or size > _MAX_WAV:
        _fail("station_rvc_output_invalid")
    with path.open("rb") as source:
        header = source.read(12)
        if len(header) != 12 or header[:4] != b"RIFF" or header[8:] != b"WAVE":
            _fail("station_rvc_output_invalid")
        if struct.unpack("<I", header[4:8])[0] != size - 8:
            _fail("station_rvc_output_invalid")
        fmt = None
        data_size = None
        data_offset = None
        cursor = 12
        while cursor + 8 <= size:
            chunk = source.read(8)
            if len(chunk) != 8:
                _fail("station_rvc_output_invalid")
            name, length = chunk[:4], struct.unpack("<I", chunk[4:])[0]
            cursor += 8
            if length > size - cursor:
                _fail("station_rvc_output_invalid")
            if name == b"fmt ":
                if fmt is not None or length < 16:
                    _fail("station_rvc_output_invalid")
                fmt = source.read(length)
            elif name == b"data":
                if data_size is not None:
                    _fail("station_rvc_output_invalid")
                data_offset, data_size = cursor, length
                source.seek(length, 1)
            else:
                source.seek(length, 1)
            cursor += length
            if length & 1:
                if cursor >= size or source.read(1) != b"\0":
                    _fail("station_rvc_output_invalid")
                cursor += 1
        if cursor != size or fmt is None or data_size is None:
            _fail("station_rvc_output_invalid")
    tag, channels, sample_rate, byte_rate, block_align, bits = struct.unpack("<HHIIHH", fmt[:16])
    if tag != 1 or bits != 16 or not 1 <= channels <= 8 or not 8000 <= sample_rate <= 384000:
        _fail("station_rvc_output_invalid")
    if block_align != channels * 2 or byte_rate != sample_rate * block_align or data_size % block_align:
        _fail("station_rvc_output_invalid")
    frames = data_size // block_align
    if frames <= 0:
        _fail("station_rvc_output_invalid")
    return {
        "frames": frames,
        "sample_rate": sample_rate,
        "channels": channels,
        "bits_per_sample": bits,
        "duration_seconds": frames / sample_rate,
        "data_offset": data_offset,
        "data_bytes": data_size,
    }


def _duration(value, code):
    if not isinstance(value, (int, float)) or isinstance(value, bool) or not math.isfinite(value) or value < 0:
        _fail(code)
    return float(value)


def _expected_profile(profile):
    if not isinstance(profile, dict):
        _fail("station_rvc_output_invalid")
    result = {
        "profile": profile.get("profile"),
        "model_sha256": profile.get("model_sha256"),
        "index_sha256": profile.get("index_sha256"),
        "requested_device": profile.get("requested_device"),
    }
    if not isinstance(result["profile"], str) or not _PROFILE.fullmatch(result["profile"]):
        _fail("station_rvc_output_invalid")
    _hash_field(result["model_sha256"], "station_rvc_output_invalid")
    _hash_field(result["index_sha256"], "station_rvc_output_invalid")
    if result["requested_device"] not in ("auto", "cpu", "cuda:0"):
        _fail("station_rvc_output_invalid")
    return result


def verify_output(state, jid, output, profile):
    """Return output metadata only after verifying exact files and WAV evidence."""
    _job_id(jid)
    state = _directory(state, "station_rvc_output_invalid")
    expected = _expected_profile(profile)
    if not isinstance(output, dict):
        _fail("station_rvc_output_invalid")
    expected_name = jid + "-rvc.wav"
    if output.get("file") not in (None, expected_name):
        _fail("station_rvc_output_invalid")
    wav = _regular(state / expected_name, "station_rvc_output_invalid", _MAX_WAV)
    sidecar, voice = _json_object(state / (jid + "-rvc.voice.json"), _MAX_SIDECAR, "station_rvc_output_invalid")
    if voice.get("profile") != expected["profile"] or str(voice.get("engine", "")).lower() != "rvc":
        _fail("station_rvc_output_invalid")
    if voice.get("model_sha256") != expected["model_sha256"] or voice.get("index_sha256") != expected["index_sha256"]:
        _fail("station_rvc_output_invalid")
    if voice.get("requested_device") != expected["requested_device"]:
        _fail("station_rvc_output_invalid")
    actual_device = voice.get("rvc_device", voice.get("device"))
    if actual_device not in ("cpu", "cuda:0"):
        _fail("station_rvc_output_invalid")
    if expected["requested_device"] != "auto" and actual_device != expected["requested_device"]:
        _fail("station_rvc_output_invalid")
    evidence = _wav_evidence(wav)
    # Both producer metadata and sidecar duration must describe the same complete
    # data chunk; a small fraction of a frame is not a meaningful tolerance.
    requested = _duration(output.get("requested_seconds"), "station_rvc_output_invalid")
    if abs(requested - evidence["duration_seconds"]) > 0.25:
        _fail("station_rvc_output_invalid")
    for claimed in (output.get("duration_seconds"), voice.get("duration_seconds")):
        if claimed is not None and abs(_duration(claimed, "station_rvc_output_invalid") - evidence["duration_seconds"]) > 0.25:
            _fail("station_rvc_output_invalid")
    if output.get("bytes") is not None and output.get("bytes") != wav.stat().st_size:
        _fail("station_rvc_output_invalid")
    result = {
        "file": expected_name,
        "bytes": wav.stat().st_size,
        "sha256": _digest(wav),
        "duration_seconds": evidence["duration_seconds"],
        "frames": evidence["frames"],
        "sample_rate": evidence["sample_rate"],
        "channels": evidence["channels"],
        "bits_per_sample": evidence["bits_per_sample"],
        "profile": expected["profile"],
        "model_sha256": expected["model_sha256"],
        "index_sha256": expected["index_sha256"],
        "requested_device": expected["requested_device"],
        "rvc_device": actual_device,
    }
    result["requested_seconds"] = requested
    if isinstance(output.get("quality"), dict):
        # Quality is descriptive only. Do not relay a converter's arbitrary
        # fields into the trusted journal.
        result["quality"] = {key: output["quality"][key] for key in (
            "duration_passed", "engine", "vocal_gender_verified",
            "timbre_listening_verified", "preserves_original_timing",
        ) if key in output["quality"]}
    # Avoid allowing a caller to mistake sidecar content for unverified output.
    del sidecar
    return result


def read_output(state, jid, job):
    """Read the one verified RVC WAV named by ``jid``; never serve a path hint."""
    _job_id(jid)
    if not isinstance(job, dict) or job.get("id", jid) != jid:
        _fail("station_rvc_output_invalid")
    profile = job.get("singing_voice", job.get("profile"))
    output = job.get("output", job.get("result", job))
    if not isinstance(output, dict):
        _fail("station_rvc_output_invalid")
    verified = verify_output(state, jid, output, profile)
    if job.get("bytes") != verified["bytes"] or job.get("sha256") != verified["sha256"]:
        _fail("station_rvc_output_invalid")
    wav = _regular(_directory(state, "station_rvc_output_invalid") / (jid + "-rvc.wav"), "station_rvc_output_invalid", _MAX_WAV)
    try:
        with wav.open("rb") as source:
            data = source.read(_MAX_WAV + 1)
    except OSError:
        _fail("station_rvc_output_invalid")
    if (len(data) != job["bytes"] or len(data) != verified["bytes"]
            or _digest_bytes(data) != job["sha256"] or _digest_bytes(data) != verified["sha256"]):
        _fail("station_rvc_output_invalid")
    return data
