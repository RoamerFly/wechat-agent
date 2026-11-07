"""Trusted broker helper for an account's offline Linux music processes.

The HTTP/token-bearing broker stays outside this namespace. Only public code,
models/runtimes and ONE account's private workspace are mounted. This helper
does not add a host-shell fallback or pass broker credentials to inference.
"""
import os
import json
import hashlib
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import time
import uuid


def _training_script(argv):
    """Locate an explicit Python script without accepting -c/-m bootstraps."""
    index = 1
    while index < len(argv):
        option = argv[index]
        if option in ("-I", "-B", "-u", "-E", "-s", "-S", "-O", "-OO"):
            index += 1
        elif option in ("-X", "-W") and index + 1 < len(argv):
            index += 2
        elif option == "--":
            return argv[index + 1] if index + 1 < len(argv) else None
        else:
            return option if not option.startswith("-") else None
    return None


def _training_object(path):
    path = canonical(path)
    if not path.is_file() or path.stat().st_size > 1 << 20:
        raise ValueError("music_training_registration_invalid")
    def unique(pairs):
        value = {}
        for key, item in pairs:
            if key in value:
                raise ValueError("music_training_registration_invalid")
            value[key] = item
        return value
    value = json.loads(path.read_text(encoding="utf8"), object_pairs_hook=unique)
    if not isinstance(value, dict):
        raise ValueError("music_training_registration_invalid")
    return value


def training_readonly(public, argv):
    """Admit only the deployed music training CLI for this explicit command.

    The Agent-editable RVC manifest is a binding, never a source of new mount
    paths: the catalog root comes from this trusted deployed runner's location.
    Other music commands receive no training-code mount.
    """
    script = _training_script(argv)
    if script is None or Path(script).name.lower() != "rvc_train_trial.py":
        return []
    control = canonical(Path(__file__).parent)
    if control.name != "windows-music":
        raise ValueError("music_training_deployed_runner_required")
    expected = canonical(control.parent.parent / "plugins/music")
    public = canonical(public)
    registration = _training_object(public / "manifests/rvc.json")
    if registration.get("training_owner") != "plugin:music" or canonical(registration.get("training_package_directory", "")) != expected:
        raise ValueError("music_training_package_not_deployed_catalog")
    # The managed interpreter also has public project paths on sys.path. Never
    # resolve this security check through an Agent-editable module shadow.
    stub = canonical(control / "rvc_train_trial.py")
    helper = {"__name__": "_managed_music_training_binding", "__file__": str(stub)}
    exec(compile(stub.read_bytes(), str(stub), "exec"), helper)
    digest = registration.get("training_package_sha256")
    if not isinstance(digest, str) or len(digest) != 64:
        raise ValueError("music_training_package_binding_invalid")
    resources = helper["training_resources"](expected, digest)
    entry, requirements = canonical(resources["entry"]), canonical(resources["requirements"])
    if canonical(registration.get("training_entry", "")) != entry or canonical(registration.get("training_requirements", "")) != requirements:
        raise ValueError("music_training_package_binding_invalid")
    candidate = canonical(script)
    if candidate != entry:
        if not candidate.is_relative_to(public / "projects") or not candidate.is_file():
            raise ValueError("music_training_cli_not_registered")
        binding = _training_object(candidate.with_name(helper["BINDING"]))
        if binding != {"version": 1, "package_directory": str(expected), "manifest_sha256": digest}:
            raise ValueError("music_training_legacy_binding_changed")
        if hashlib.sha256(candidate.read_bytes()).digest() != hashlib.sha256(stub.read_bytes()).digest():
            raise ValueError("music_training_legacy_cli_changed")
    return [str(expected)]


def canonical(path):
    path = Path(path)
    if not path.is_absolute():
        raise ValueError("music_workspace_requires_absolute_path")
    for parent in [path, *path.parents]:
        if parent.is_symlink() or getattr(parent, "is_junction", lambda: False)():
            raise ValueError("music_workspace_link_not_allowed")
    return path.resolve(strict=True)


def command(public, private, cwd, argv, gpu=False, ipc=None):
    if sys.platform != "linux" or not shutil.which("bwrap"):
        raise RuntimeError("music_sandbox_unavailable")
    public, private, cwd = map(canonical, (public, private, cwd))
    if public.name != "public_workspace" or private.parent.name != "pri_workspace" or public.parent != private.parent.parent:
        raise ValueError("music_managed_layout_required")
    if not (cwd.is_relative_to(public) or cwd.is_relative_to(private)):
        raise ValueError("music_working_directory_outside_workspace")
    if not argv or any(not isinstance(x, str) or "\0" in x for x in argv):
        raise ValueError("music_argv_invalid")
    if not Path(argv[0]).is_absolute():
        raise ValueError("music_interpreter_requires_absolute_path")
    # Venv interpreters are normally symlinks into another managed runtime.
    # The target must still reside inside the mounted runtime or system /usr.
    binary = Path(argv[0]).resolve(strict=True)
    if not (binary.is_relative_to(public / "runtimes") or binary.is_relative_to(Path("/usr"))):
        raise ValueError("music_interpreter_not_managed")
    scratch = private / "scratch"
    scratch.mkdir(exist_ok=True)
    args = [shutil.which("bwrap"), "--die-with-parent", "--new-session", "--unshare-all", "--cap-drop", "ALL", "--clearenv"]
    for name in ("/usr", "/bin", "/sbin", "/lib", "/lib64"):
        path = Path(name)
        if path.is_symlink():
            args += ["--symlink", os.readlink(path), name]
        elif path.exists():
            args += ["--ro-bind", name, name]
    args += ["--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--tmpfs", "/dev/shm"]
    if Path("/etc/ld.so.cache").exists():
        args += ["--ro-bind", "/etc/ld.so.cache", "/etc/ld.so.cache"]
    # Debian/Ubuntu BLAS/LAPACK sonames point through /etc/alternatives.
    # Recreate only library links whose targets are already in read-only /usr
    # or /lib, rather than exposing the host's whole /etc directory.
    alternatives = Path('/etc/alternatives')
    links = []
    for alias in sorted(alternatives.glob('lib*.so*')):
        if not alias.is_symlink(): continue
        try: target = alias.resolve(strict=True)
        except OSError: continue
        if not any(target.is_relative_to(Path(base)) for base in ('/usr/lib','/lib')): continue
        links += ['--symlink',os.readlink(alias),str(alias)]
    if links: args += ['--dir',str(alternatives),*links]
    if gpu:
        devices = [name for name in ("/dev/dxg", "/dev/nvidia0", "/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools") if Path(name).exists()]
        if not devices:
            raise RuntimeError("music_gpu_device_unavailable")
        for name in devices:
            args += ["--dev-bind", name, name]
    args += ["--ro-bind", str(public), str(public), "--bind", str(private), str(private)]
    if ipc is not None:
        # A dedicated, owner-only socket directory; never bind the broker's
        # state/token directory. DrvFS cannot host Unix sockets, so this narrow
        # IPC mount lives on the Linux filesystem instead.
        ipc = canonical(ipc)
        if ipc.stat().st_uid != os.getuid() or ipc.stat().st_mode & 0o077:
            raise ValueError("music_ipc_requires_private_directory")
        if any(not path.is_socket() for path in ipc.iterdir()):
            raise ValueError("music_ipc_contains_non_socket_files")
        args += ["--bind", str(ipc), "/run/managed-music"]
    env = {"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": str(scratch), "TMPDIR": "/tmp", "LANG": "C.UTF-8",
           "HF_HOME": str(scratch / "hf-cache"), "TORCH_HOME": str(scratch / "torch-cache"),
           "HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1", "PYTHONDONTWRITEBYTECODE": "1",
           "LD_LIBRARY_PATH": "/usr/lib/wsl/lib:/usr/lib/x86_64-linux-gnu"}
    for key, value in env.items():
        args += ["--setenv", key, value]
    return args + ["--chdir", str(cwd), "--", *argv]


def run(public, private, cwd, argv, *, gpu=False, timeout=None, ipc=None, cancelled=None):
    if sys.platform == "win32":
        if cancelled is not None and cancelled():
            raise RuntimeError("station_rvc_interrupted")
        options = {"station_rvc": True} if cancelled is not None else {}
        child = launch_windows(public, private, cwd, argv, gpu=gpu, **options)
        try:
            code = wait_windows(child, timeout, cancelled)
            if code:
                raise subprocess.CalledProcessError(code, argv)
        except BaseException:
            if child.poll() is None:
                child.kill()
                child.wait()
            raise
        return
    if cancelled is not None:
        raise RuntimeError("station_rvc_windows_required")
    child = subprocess.Popen(command(public, private, cwd, argv, gpu, ipc), env={"PATH": "/usr/bin:/bin"}, start_new_session=True)
    try:
        code = child.wait(timeout=timeout)
        if code:
            raise subprocess.CalledProcessError(code, argv)
    except BaseException:
        if child.poll() is None:
            os.killpg(child.pid, signal.SIGKILL)
            child.wait()
        raise


def wait_windows(child, timeout, cancelled):
    if cancelled is None:
        return child.wait(timeout=timeout)
    deadline = time.monotonic() + timeout if timeout is not None else None
    while True:
        if cancelled():
            # The caller kills the exact owned LPAC Job, including descendants.
            raise RuntimeError("station_rvc_interrupted")
        remaining = deadline - time.monotonic() if deadline is not None else None
        if remaining is not None and remaining <= 0:
            raise subprocess.TimeoutExpired("station-rvc", timeout)
        try:
            return child.wait(timeout=min(0.25, remaining) if remaining is not None else 0.25)
        except subprocess.TimeoutExpired:
            continue


def launch_windows(public, private, cwd, argv, *, gpu=False, station_rvc=False):
    """Trusted music-only policy; ordinary Agent broker has no such switch."""
    if sys.platform != "win32" or os.environ.get("WECHAT_WINDOWS_MUSIC_POLICY") != "registry-read-private-network-v1":
        raise RuntimeError("windows_music_policy_not_authorized")
    public, private, cwd = map(canonical, (public, private, cwd))
    if public.name != "public_workspace" or private.parent.name != "pri_workspace" or public.parent != private.parent.parent:
        raise ValueError("music_managed_layout_required")
    from music_account_scope import account_scope
    account_scope(private)
    if not cwd.is_relative_to(private):
        raise ValueError("music_working_directory_outside_account")
    if not argv or any(not isinstance(x, str) or "\0" in x for x in argv):
        raise ValueError("music_argv_invalid")
    binary = canonical(argv[0])
    if not binary.is_relative_to(public / "runtimes"):
        raise ValueError("music_interpreter_not_managed")
    training = training_readonly(public, argv)
    readonly = [str(public)]
    if station_rvc:
        control = canonical(Path(__file__).parent)
        if (control.name != "windows-station-rvc" or control.is_relative_to(public.parent) or
                len(argv) < 2 or canonical(argv[1]) != control / "singing_voice.py"):
            raise ValueError("station_rvc_converter_not_packaged")
        readonly.append(str(control))
    from windows_lpac import launch
    media = canonical(os.environ["WECHAT_WINDOWS_MUSIC_MEDIA"])
    if not (media / "ffmpeg.exe").is_file() or not (media / "ffprobe.exe").is_file():
        raise ValueError("windows_music_media_unavailable")
    scratch = private / "scratch"
    scratch.mkdir(exist_ok=True)
    log = private / "music/runtime-logs" / uuid.uuid4().hex
    log.mkdir(parents=True)
    environment = {"PATH": str(media) + os.pathsep + str(Path(os.environ["SystemRoot"]) / "System32"),
                   "HF_HOME": str(scratch / "hf-cache"), "HF_HUB_CACHE": str(scratch / "hf-cache/hub"),
                   "TORCH_HOME": str(scratch / "torch-cache"), "HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1",
                   "PYTHONDONTWRITEBYTECODE": "1", "PYTHONUTF8": "1", "MPLCONFIGDIR": str(scratch / "matplotlib"),
                   "NUMBA_CACHE_DIR": str(scratch / "numba-cache")}
    environment["WECHAT_WINDOWS_MUSIC_RUNTIME"] = "1"
    environment["USERNAME"] = "managed-music"
    with (log / "stdout.log").open("wb") as output, (log / "stderr.log").open("wb") as error:
        child = launch(argv, str(cwd), [str(private)], [*readonly, str(media), *training], output, error,
                       music_runtime=True, environment=environment)
        child.log_directory = log
        return child


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument("--public", required=True)
    parser.add_argument("--private", required=True)
    parser.add_argument("--cwd", required=True)
    parser.add_argument("--gpu", action="store_true")
    parser.add_argument("--ipc")
    parser.add_argument("argv", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    argv = args.argv[1:] if args.argv[:1] == ["--"] else args.argv
    run(args.public, args.private, args.cwd, argv, gpu=args.gpu, ipc=args.ipc)
