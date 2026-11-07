"""Private file transport for WSL -> Windows execution without a console.
Only launched by the owner-authorized host tool; never an HTTP server.
"""
import base64
import ctypes
from ctypes import wintypes
import hashlib
import json
import math
import os
from pathlib import Path, PurePosixPath
import subprocess
import sys
import threading
import time

# Embedded Python's isolated ._pth intentionally omits arbitrary script paths.
sys.path.insert(0, str(Path(__file__).resolve().parent))


def _verified_parent_handle(request):
    """Return a waitable handle for the exact native host process.

    A PID is reusable.  The Go host therefore also supplies the kernel
    creation FILETIME; this prevents a newly-created unrelated process from
    being accepted after the original host has died.  The returned handle is
    deliberately kept open until broker exit, so the watchdog has no polling
    window and closing this broker closes its LPAC job handle.
    """
    parent = request.get("parent_pid")
    created = request.get("parent_created_filetime")
    if type(parent) is not int or parent <= 0 or type(created) is not int or created <= 0:
        raise ValueError("invalid_broker_parent_identity")
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
    kernel.OpenProcess.restype = wintypes.HANDLE
    kernel.GetProcessTimes.argtypes = [wintypes.HANDLE, ctypes.POINTER(wintypes.FILETIME), ctypes.POINTER(wintypes.FILETIME), ctypes.POINTER(wintypes.FILETIME), ctypes.POINTER(wintypes.FILETIME)]
    kernel.GetProcessTimes.restype = wintypes.BOOL
    kernel.WaitForSingleObject.argtypes = [wintypes.HANDLE, wintypes.DWORD]
    kernel.WaitForSingleObject.restype = wintypes.DWORD
    kernel.CloseHandle.argtypes = [wintypes.HANDLE]
    kernel.CloseHandle.restype = wintypes.BOOL
    # SYNCHRONIZE | PROCESS_QUERY_LIMITED_INFORMATION.  The latter is only
    # used to bind the PID to its creation time and does not grant mutation.
    handle = kernel.OpenProcess(0x00100000 | 0x1000, False, parent)
    if not handle:
        raise RuntimeError("broker_parent_unavailable")
    creation, exited, kernel_time, user_time = (wintypes.FILETIME() for _ in range(4))
    if not kernel.GetProcessTimes(handle, ctypes.byref(creation), ctypes.byref(exited), ctypes.byref(kernel_time), ctypes.byref(user_time)):
        kernel.CloseHandle(handle)
        raise RuntimeError("broker_parent_unavailable")
    actual = (int(creation.dwHighDateTime) << 32) | int(creation.dwLowDateTime)
    if actual != created or kernel.WaitForSingleObject(handle, 0) != 258:  # WAIT_TIMEOUT
        kernel.CloseHandle(handle)
        raise RuntimeError("broker_parent_unavailable")
    return kernel, handle


def _watch_parent(request, required):
    # The Python unit tests import this module on non-Windows hosts.  The
    # production broker is Windows-only, and must never launch without this
    # check there.
    if os.name != "nt":
        return
    has_pid, has_created = "parent_pid" in request, "parent_created_filetime" in request
    if not has_pid and not has_created and not required:
        # Deployed generic brokers may still be launched by the WSL host. It
        # is not the native Windows parent and cannot supply a waitable handle.
        return
    if not has_pid or not has_created:
        raise ValueError("invalid_broker_parent_identity")
    kernel, handle = _verified_parent_handle(request)
    def parent_exited():
        kernel.WaitForSingleObject(handle, 0xffffffff)
        # The LPAC job is KILL_ON_JOB_CLOSE.  Do not publish a receipt after
        # the owning host is gone: the caller must treat this as unknown and
        # reconcile through its durable idempotency record.
        os._exit(1)
    threading.Thread(target=parent_exited, daemon=True).start()


def _scrub_offline_speech_request(path, request):
    """Remove transient synthesis text after this broker owns it in memory.

    The request spool is a control record, not a long-term transcript.  Keep
    only the policy and parent binding needed to diagnose a crashed current
    run; do not retain argv (which may contain the private text to speak).
    Historical request directories are deliberately untouched.
    """
    audit = {
        "sandbox": True,
        "music_policy": "registry-read-offline-speech-v1",
        "parent_pid": request["parent_pid"],
        "parent_created_filetime": request["parent_created_filetime"],
        "scrubbed": "offline_speech_request_arguments",
    }
    pending = path.with_name("request.scrubbed.pending")
    pending.write_text(json.dumps(audit, separators=(",", ":")), encoding="utf8")
    pending.replace(path)


def _training_path(path):
    path = Path(path)
    if not path.is_absolute(): raise ValueError("training_profile_absolute_path_required")
    for part in (path, *path.parents):
        if part.is_symlink() or getattr(part, "is_junction", lambda: False)():
            raise ValueError("training_profile_link_forbidden")
    return path.resolve(strict=True)


def _training_json(path):
    path = _training_path(path)
    if path.stat().st_size > 1 << 20: raise ValueError("training_profile_json_limit")
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result: raise ValueError("training_profile_duplicate_field")
            result[key] = value
        return result
    value = json.loads(path.read_text(encoding="utf8"), object_pairs_hook=unique)
    if not isinstance(value, dict): raise ValueError("training_profile_object_required")
    return value


def _training_runner():
    own = _training_path(__file__)
    if own.parent.name != "windows-agent-host": raise ValueError("training_profile_deployment_required")
    controls = _training_path(own.parent.parent / "windows-music")
    manifest = _training_json(controls / "runtime-manifest.json")
    if manifest.get("schema_version") != 1 or manifest.get("policy") != "registry-read-private-network-v1" or manifest.get("platform") != "windows-x64" or not isinstance(manifest.get("files"), dict):
        raise ValueError("training_controls_manifest_invalid")
    expected = {"runtime-manifest.json"}
    for name, checksum in manifest["files"].items():
        member = PurePosixPath(name.replace("\\", "/"))
        normalized = member.as_posix()
        if not name or ":" in name or member.is_absolute() or normalized != name.replace("\\", "/") or ".." in member.parts or normalized in expected:
            raise ValueError("training_controls_index_invalid")
        resource = _training_path(controls.joinpath(*member.parts))
        if not resource.is_file() or not isinstance(checksum, str) or hashlib.sha256(resource.read_bytes()).hexdigest() != checksum:
            raise ValueError("training_controls_member_changed")
        expected.add(normalized)
    found = set()
    for base, directories, names in os.walk(controls, followlinks=False):
        for name in directories + names: _training_path(Path(base) / name)
        found.update((Path(base) / name).relative_to(controls).as_posix() for name in names)
    if found != expected or not {"managed_music_runner.py", "rvc_train_trial.py", "managed_music_service.py"}.issubset(expected):
        raise ValueError("training_controls_unindexed_or_missing")
    entry = controls / "managed_music_runner.py"
    namespace = {"__file__": str(entry), "__name__": "_deployed_managed_music_runner"}
    exec(compile(entry.read_bytes(), str(entry), "exec"), namespace)
    return controls, namespace


def _native_training_argv(command):
    # Accept one native Python command, never a shell program/expansion. Quoted
    # argument data follows Windows argv rules and is passed unchanged to LPAC.
    import ctypes
    from ctypes import wintypes
    shell = ctypes.WinDLL("shell32", use_last_error=True)
    shell.CommandLineToArgvW.argtypes = [wintypes.LPCWSTR, ctypes.POINTER(ctypes.c_int)]
    shell.CommandLineToArgvW.restype = ctypes.POINTER(wintypes.LPWSTR)
    count = ctypes.c_int()
    pointer = shell.CommandLineToArgvW(command.lstrip(), ctypes.byref(count))
    if not pointer: raise ValueError("training_command_argv_invalid")
    try: return [pointer[index] for index in range(count.value)]
    finally:
        kernel = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel.LocalFree.argtypes = [wintypes.HLOCAL]
        kernel.LocalFree(ctypes.cast(pointer, wintypes.HLOCAL))


def _single_training_command(command):
    quoted, escaped = False, False
    for character in command:
        if character == '"' and not escaped: quoted = not quoted
        if character in "%$`\r\n" or not quoted and character in "&|<>^;":
            raise ValueError("training_command_requires_single_native_argv")
        escaped = character == "\\" and not escaped
    if quoted: raise ValueError("training_command_unclosed_quote")


def training_mount(request, direct, command):
    candidate = direct
    if candidate is None:
        if "rvc_train_trial.py" not in command.lower(): return direct, []
        candidate = _native_training_argv(command)
    # Locate only an explicit script. -c/-m code and ordinary commands never
    # gain a training mount even if a string mentions the deployed catalog.
    if len(candidate) < 2 or Path(candidate[0]).name.lower() not in ("python.exe", "pythonw.exe"):
        return direct, []
    index = 1
    while index < len(candidate):
        option = candidate[index]
        if option in ("-I", "-B", "-u", "-E", "-s", "-S", "-O", "-OO"): index += 1
        elif option in ("-X", "-W") and index + 1 < len(candidate): index += 2
        elif option == "--": index += 1; break
        else: break
    if index >= len(candidate) or Path(candidate[index]).name.lower() != "rvc_train_trial.py": return direct, []
    if direct is None: _single_training_command(command)
    if request.get("gpu_authorized") is not True or not request.get("music_registration"):
        raise ValueError("training_registered_gpu_profile_required")
    controls, runner = _training_runner()
    profile = _training_json(request["music_registration"])
    if profile.get("enabled") is not True or profile.get("policy") != "registry-read-private-network-v1" or _training_path(profile["entry"]) != controls / "managed_music_service.py":
        raise ValueError("training_registered_gpu_profile_required")
    interpreter = _training_path(candidate[0])
    if _training_path(profile["python"]) != interpreter: raise ValueError("training_registered_interpreter_required")
    # The compact workspace layout deliberately mounts public/runtimes as RO
    # and public/projects separately as RW.  Do not require (or add) the whole
    # public workspace just to recognise a registered interpreter.  Conversely,
    # a similarly named editable project directory cannot become a runtime
    # merely by being supplied as a request root.
    runtime = next((parent for parent in (interpreter.parent, *interpreter.parents)
                    if parent.name == "runtimes" and parent.parent.name == "public_workspace"), None)
    if runtime is None or not interpreter.is_relative_to(runtime):
        raise ValueError("training_registered_interpreter_required")
    public = runtime.parent
    readonly = {_training_path(path) for path in request.get("read_only", [])}
    # The legacy full public mount remains compatible, but the new layout must
    # name its exact runtime child.  A writable root is never accepted as this
    # proof, even if it happens to contain the expected directory tree.
    if public not in readonly and runtime not in readonly:
        raise ValueError("training_registered_public_workspace_required")
    return candidate, runner["training_readonly"](public, candidate)


def run(request_path):
    path = Path(request_path)
    directory = path.parent
    result = {"exit_code": -1}
    try:
        request = json.loads(path.read_text(encoding="utf8"))
        if request.get("sandbox") is not True: raise RuntimeError("sandbox_required: unrestricted Windows execution is disabled")
        direct = request.get("argv")
        if direct is not None and (not isinstance(direct, list) or not direct or any(not isinstance(x, str) or "\0" in x for x in direct) or not Path(direct[0]).is_absolute()):
            raise ValueError("invalid_isolated_argv")
        command = request.get("command", "")
        # The historical field name is kept for the authenticated Go broker
        # protocol.  It now carries one of two fixed runtime policies, never
        # an Agent-provided capability list.
        policy = request.get("music_policy", "")
        accepted_policies = {"registry-read-private-network-v1", "registry-read-offline-speech-v1"}
        if policy and (policy not in accepted_policies or direct is None or command):
            raise ValueError("invalid_music_runtime_policy")
        if request.get("environment") and not policy:
            raise ValueError("music_runtime_environment_requires_authorized_policy")
        timeout = request.get("timeout_seconds")
        if timeout is not None and (isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or not math.isfinite(timeout) or timeout < 0):
            raise ValueError("invalid_execution_timeout")
        _watch_parent(request, policy == "registry-read-offline-speech-v1")
        deadline = time.monotonic() + timeout if timeout else None
        if policy == "registry-read-offline-speech-v1":
            _scrub_offline_speech_request(path, request)
        direct, training = training_mount(request, direct, command)
        if (directory / "cancel").exists() or deadline is not None and time.monotonic() >= deadline:
            result["cancelled"] = (directory / "cancel").exists()
            result["timed_out"] = deadline is not None and time.monotonic() >= deadline
            raise RuntimeError("execution_cancelled_or_timed_out_before_start")
        # Preserve Unicode and native nonzero exit codes without bash/$ escaping.
        script = "$ErrorActionPreference='Stop'; $global:LASTEXITCODE=0; "
        script += "[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false); $OutputEncoding=[Console]::OutputEncoding; "
        script += "try { & {\n" + command + "\n}; exit $LASTEXITCODE } catch { [Console]::Error.WriteLine($_.Exception.Message); exit 1 }"
        encoded = base64.b64encode(script.encode("utf-16le")).decode("ascii")
        flags = getattr(subprocess, "CREATE_NO_WINDOW", 0) | getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0)
        with (directory / "output.log").open("wb") as output, (directory / "stderr.log").open("wb") as diagnostic:
            from windows_lpac import launch
            if direct is not None:
                argv = direct
            elif Path(request["shell"]).name.lower() == "cmd.exe":
                argv=[request["shell"],"/d","/s","/c",command]
            else:
                argv=[request["shell"],"-NoProfile","-NonInteractive","-WindowStyle","Hidden","-EncodedCommand",encoded]
            process = launch(argv, request["cwd"], request["roots"], [*request.get("read_only", []), *training], output, diagnostic if direct is not None else None,
                             music_runtime=policy == "registry-read-private-network-v1",
                             speech_runtime=policy == "registry-read-offline-speech-v1",
                             environment=request.get("environment"))
            (directory / "running.json").write_text(json.dumps({"pid": process.pid, "helper_pid": os.getpid(), "started": time.time()}), encoding="utf8")
            while process.poll() is None:
                if (directory / "cancel").exists() or deadline is not None and time.monotonic() >= deadline:
                    process.kill()
                    process.wait(timeout=5)
                    result["cancelled"] = (directory / "cancel").exists()
                    result["timed_out"] = deadline is not None and time.monotonic() >= deadline
                    break
                time.sleep(.1)
            result["exit_code"] = process.returncode
    except Exception as error:
        # Only a local diagnostic. Command/output files stay in the private spool.
        result["error"] = type(error).__name__ + ": " + str(error)
    result["finished"] = time.time()
    pending = directory / "result.pending"
    pending.write_text(json.dumps(result), encoding="utf8")
    pending.replace(directory / "result.json")


if __name__ == "__main__":
    run(sys.argv[1])
