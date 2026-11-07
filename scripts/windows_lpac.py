"""Launch a process in a low privilege AppContainer. No unrestricted fallback.

The caller is a trusted broker, not Agent code. Paths are broker-derived. This
module never grants broad capabilities or exposes management credentials.
"""
import ctypes as c
from ctypes import wintypes as w
import hashlib
import os
from pathlib import Path
import subprocess
import time


class SECURITY_CAPABILITIES(c.Structure):
    _fields_ = [("AppContainerSid", c.c_void_p), ("Capabilities", c.c_void_p),
                ("CapabilityCount", w.DWORD), ("Reserved", w.DWORD)]


class STARTUPINFO(c.Structure):
    _fields_ = [("cb", w.DWORD), ("lpReserved", w.LPWSTR), ("lpDesktop", w.LPWSTR),
                ("lpTitle", w.LPWSTR), ("dwX", w.DWORD), ("dwY", w.DWORD),
                ("dwXSize", w.DWORD), ("dwYSize", w.DWORD), ("dwXCountChars", w.DWORD),
                ("dwYCountChars", w.DWORD), ("dwFillAttribute", w.DWORD),
                ("dwFlags", w.DWORD), ("wShowWindow", w.WORD), ("cbReserved2", w.WORD),
                ("lpReserved2", c.c_void_p), ("hStdInput", w.HANDLE),
                ("hStdOutput", w.HANDLE), ("hStdError", w.HANDLE)]


class STARTUPINFOEX(c.Structure):
    _fields_ = [("StartupInfo", STARTUPINFO), ("lpAttributeList", c.c_void_p)]


class PROCESS_INFORMATION(c.Structure):
    _fields_ = [("hProcess", w.HANDLE), ("hThread", w.HANDLE),
                ("dwProcessId", w.DWORD), ("dwThreadId", w.DWORD)]


def mount_granted(path, sid, mode):
    """Avoid recursively repropagating an unchanged root ACE on every job."""
    adv = c.WinDLL("advapi32", use_last_error=True)
    kernel = c.WinDLL("kernel32")
    dacl, descriptor = c.c_void_p(), c.c_void_p()
    adv.GetNamedSecurityInfoW.argtypes = [w.LPWSTR, c.c_int, w.DWORD, c.c_void_p, c.c_void_p,
                                        c.POINTER(c.c_void_p), c.c_void_p, c.POINTER(c.c_void_p)]
    adv.GetAclInformation.argtypes = [c.c_void_p, c.c_void_p, w.DWORD, c.c_int]
    adv.GetAce.argtypes = [c.c_void_p, w.DWORD, c.POINTER(c.c_void_p)]
    adv.EqualSid.argtypes = [c.c_void_p, c.c_void_p]
    kernel.LocalFree.argtypes = [c.c_void_p]
    if adv.GetNamedSecurityInfoW(str(path), 1, 4, None, None, c.byref(dacl), None, c.byref(descriptor)):
        return False
    try:
        info = (w.DWORD * 3)()
        if not dacl or not adv.GetAclInformation(dacl, c.byref(info), c.sizeof(info), 2):
            return False
        expected = 0x1301BF if mode == "M" else 0x1200A9
        for i in range(info[0]):
            ace = c.c_void_p()
            if not adv.GetAce(dacl, i, c.byref(ace)):
                return False
            flags = c.c_ubyte.from_address(ace.value + 1).value
            kind = c.c_ubyte.from_address(ace.value).value
            mask = w.DWORD.from_address(ace.value + 4).value
            inherited = flags & 3 == 3
            if kind == 0 and (inherited if Path(path).is_dir() else not inherited) and mask & expected == expected and adv.EqualSid(ace.value + 8, sid):
                return True
        return False
    finally:
        kernel.LocalFree(descriptor)


def launch(argv, cwd, writable, readonly, output, error_output=None, *, music_runtime=False, speech_runtime=False, environment=None, input_stream=None):
    """Return a process handle, pid and cleanup. Child inherits only stdio.

    LPAC additionally opts out of implicit ALL APPLICATION PACKAGES grants.
    Each distinct mount policy receives its own SID, so ACLs from a previous
    administrator operation cannot widen an ordinary session's policy.
    """
    if os.name != "nt":
        raise RuntimeError("windows_lpac_unavailable")
    import msvcrt
    k = c.WinDLL("kernel32", use_last_error=True)
    u = c.WinDLL("userenv", use_last_error=True)
    adv = c.WinDLL("advapi32", use_last_error=True)
    k.GetCurrentProcess.restype = w.HANDLE
    k.CloseHandle.argtypes = [w.HANDLE]
    k.LocalFree.argtypes=[c.c_void_p]
    adv.FreeSid.argtypes=[c.c_void_p]
    k.TerminateProcess.argtypes=[w.HANDLE,w.UINT]
    k.GetExitCodeProcess.argtypes=[w.HANDLE,c.POINTER(w.DWORD)]
    k.TerminateJobObject.argtypes=[w.HANDLE,w.UINT]
    k.CreateJobObjectW.argtypes=[c.c_void_p,w.LPCWSTR]
    k.CreateJobObjectW.restype=w.HANDLE
    k.SetInformationJobObject.argtypes=[w.HANDLE,c.c_int,c.c_void_p,w.DWORD]
    k.AssignProcessToJobObject.argtypes=[w.HANDLE,w.HANDLE]
    k.ResumeThread.argtypes=[w.HANDLE]
    k.InitializeProcThreadAttributeList.argtypes = [c.c_void_p, w.DWORD, w.DWORD, c.POINTER(c.c_size_t)]
    k.UpdateProcThreadAttribute.argtypes = [c.c_void_p, w.DWORD, c.c_size_t, c.c_void_p, c.c_size_t, c.c_void_p, c.c_void_p]
    k.DeleteProcThreadAttributeList.argtypes = [c.c_void_p]
    k.CreateProcessW.argtypes = [w.LPCWSTR, w.LPWSTR, c.c_void_p, c.c_void_p, w.BOOL,
                                w.DWORD, c.c_void_p, w.LPCWSTR, c.POINTER(STARTUPINFOEX), c.POINTER(PROCESS_INFORMATION)]
    u.CreateAppContainerProfile.argtypes = [w.LPCWSTR, w.LPCWSTR, w.LPCWSTR, c.c_void_p, w.DWORD, c.POINTER(c.c_void_p)]
    u.DeriveAppContainerSidFromAppContainerName.argtypes = [w.LPCWSTR, c.POINTER(c.c_void_p)]
    adv.ConvertSidToStringSidW.argtypes = [c.c_void_p, c.POINTER(w.LPWSTR)]
    sid = c.c_void_p()
    for original in [*writable, *readonly]:
        path = Path(original)
        for parent in [path, *path.parents]:
            if parent.is_symlink() or parent.is_junction():
                raise RuntimeError("lpac_mount_reparse_point")
    paths = [(str(Path(p).resolve(strict=True)), "M") for p in writable]
    paths += [(str(Path(p).resolve(strict=True)), "RX") for p in readonly]
    for p, _ in paths:
        if Path(p).parent == Path(p) or Path(p).is_junction() or Path(p).is_symlink():
            raise RuntimeError("lpac_invalid_mount")
    if type(music_runtime) is not bool or type(speech_runtime) is not bool or music_runtime and speech_runtime:
        raise ValueError("invalid_music_runtime_policy")
    # Keep legacy generic/music identities byte-for-byte stable: their ACLs
    # and runtime caches are already bound to this hash.  Offline speech gets
    # a separate namespace so its registry-only capability can never reuse a
    # music container which has private-network access.
    if speech_runtime:
        name = "WechatAgent.Speech." + hashlib.sha256(repr((sorted(paths), speech_runtime)).encode()).hexdigest()[:32]
    else:
        name = "WechatAgent." + hashlib.sha256(repr((sorted(paths), music_runtime)).encode()).hexdigest()[:32]
    hr = u.CreateAppContainerProfile(name, name, "Managed WeChat workspace", None, 0, c.byref(sid))
    if hr != 0 and u.DeriveAppContainerSidFromAppContainerName(name, c.byref(sid)) != 0:
        raise RuntimeError("lpac_profile_unavailable")
    sid_text = w.LPWSTR()
    if not adv.ConvertSidToStringSidW(sid, c.byref(sid_text)):
        raise c.WinError(c.get_last_error())
    try:
        package_sid = sid_text.value
        for path, mode in paths:
            if mount_granted(path, sid, mode):
                continue
            # No /T: directory grants inherit to descendants; a registered
            # sidecar file receives only its own RX ACE.
            inheritance = "(OI)(CI)" if Path(path).is_dir() else ""
            subprocess.run([str(Path(os.environ["SystemRoot"]) / "System32/icacls.exe"), path,
                            "/grant", "*" + sid_text.value + ":" + inheritance + "(" + mode + ")", "/Q"], check=True,
                           stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                           creationflags=subprocess.CREATE_NO_WINDOW)
    finally:
        k.LocalFree(c.cast(sid_text, c.c_void_p))
    size = c.c_size_t()
    k.InitializeProcThreadAttributeList(None, 3, 0, c.byref(size))
    buf = c.create_string_buffer(size.value)
    if not k.InitializeProcThreadAttributeList(buf, 3, 0, c.byref(size)):
        raise c.WinError(c.get_last_error())
    si = STARTUPINFOEX()
    si.StartupInfo.cb = c.sizeof(si)
    si.lpAttributeList = c.cast(buf, c.c_void_p)
    # Only fixed, trusted broker policies may request capabilities.  Offline
    # speech may read registry configuration but receives no network capability.
    capabilities = MusicCapabilities() if music_runtime else SpeechCapabilities() if speech_runtime else None
    caps = SECURITY_CAPABILITIES(sid, capabilities.pointer if capabilities else None,
                                 capabilities.count if capabilities else 0, 0)
    optout = w.DWORD(1)
    handle = msvcrt.get_osfhandle(output.fileno())
    os.set_handle_inheritable(handle, True)
    error_handle = msvcrt.get_osfhandle(error_output.fileno()) if error_output else handle
    os.set_handle_inheritable(error_handle, True)
    stdin = input_stream if input_stream is not None else open(os.devnull, "rb")
    input_handle = msvcrt.get_osfhandle(stdin.fileno())
    os.set_handle_inheritable(input_handle, True)
    inherited = list(dict.fromkeys([handle, error_handle, input_handle]))
    handles = (w.HANDLE * len(inherited))(*inherited)
    try:
        for key, value in [(0x20009, caps), (0x2000F, optout), (0x20002, handles)]:
            if not k.UpdateProcThreadAttribute(buf, 0, key, c.byref(value), c.sizeof(value), None, None):
                raise c.WinError(c.get_last_error())
        si.StartupInfo.dwFlags = 0x100
        si.StartupInfo.hStdInput = input_handle
        si.StartupInfo.hStdOutput = handle
        si.StartupInfo.hStdError = error_handle
        # Explicit clean environment, not inherited credentials/WSL interop.
        # The managed interpreter's own directory is an already read-only
        # mounted runtime root. Keep it on PATH so its bundled CPython/Pillow
        # extension modules can resolve their matching VCRuntime/Python DLLs;
        # do not inherit a developer, user, or system-wide Python path.
        managed_python_dir = str(Path(argv[0]).resolve(strict=True).parent)
        env = {"LOCALAPPDATA":str(Path(writable[0])/"scratch"),"APPDATA":str(Path(writable[0])/"scratch"),"USERPROFILE":str(writable[0]),"SystemDrive":os.environ.get("SystemDrive","C:"),"ALLUSERSPROFILE":os.environ.get("ProgramData",r"C:\ProgramData"),"ProgramData":os.environ.get("ProgramData",r"C:\ProgramData"),"SystemRoot": os.environ["SystemRoot"], "WINDIR": os.environ["SystemRoot"],
               "PATH": managed_python_dir + os.pathsep + str(Path(os.environ["SystemRoot"]) / "System32"),
               "TEMP": str(Path(writable[0]) / "scratch"), "TMP": str(Path(writable[0]) / "scratch")}
        if environment:
            permitted = {"DOCUMENT_RUNTIME_ROOT"}
            if music_runtime or speech_runtime:
                permitted.update({"PATH", "HF_HOME", "HF_HUB_CACHE", "TORCH_HOME", "HF_HUB_OFFLINE",
                         "NLTK_DATA",
                         "TRANSFORMERS_OFFLINE", "PYTHONDONTWRITEBYTECODE", "PYTHONUTF8",
                         "PYTORCH_CUDA_ALLOC_CONF", "MPLCONFIGDIR", "NUMBA_CACHE_DIR", "WECHAT_WINDOWS_MUSIC_RUNTIME",
                         "WECHAT_WINDOWS_SPEECH_RUNTIME", "USERNAME"})
            if set(environment) - permitted or any(not isinstance(v, str) or "\0" in v for v in environment.values()):
                raise ValueError("invalid_music_runtime_environment")
            env.update(environment)
        Path(env["TEMP"]).mkdir(exist_ok=True)
        block = c.create_unicode_buffer("\0".join(k + "=" + v for k, v in sorted(env.items())) + "\0\0")
        pi = PROCESS_INFORMATION()
        line=subprocess.list2cmdline(argv)
        if Path(argv[0]).name.lower()=="cmd.exe" and len(argv)==5:
            line=subprocess.list2cmdline(argv[:4])+' "'+argv[4]+'"'
        # CreateProcessW allows at most 32,767 UTF-16 code units including
        # the terminator. Check the exact rendered command line instead of a
        # made-up argument-count quota or silent truncation.
        if len(line.encode("utf-16-le")) // 2 >= 32767:
            raise ValueError("windows_command_line_too_long")
        cmd = c.create_unicode_buffer(line)
        if not k.CreateProcessW(argv[0], cmd, None, None, True, 0x08000000 | 0x80000 | 0x400 | 4,
                                block, str(cwd), c.byref(si), c.byref(pi)):
            raise c.WinError(c.get_last_error())
        job=k.CreateJobObjectW(None,None)
        limits=EXTENDED_LIMIT();limits.BasicLimitInformation.LimitFlags=0x2000
        if not job or not k.SetInformationJobObject(job,9,c.byref(limits),c.sizeof(limits)) or not k.AssignProcessToJobObject(job,pi.hProcess):
            k.TerminateProcess(pi.hProcess,1);k.CloseHandle(pi.hThread);k.CloseHandle(pi.hProcess)
            if job:k.CloseHandle(job)
            raise RuntimeError("lpac_job_isolation_failed")
        k.ResumeThread(pi.hThread);k.CloseHandle(pi.hThread)
        process = SandboxedProcess(k,pi.hProcess,job,pi.dwProcessId)
        process.package_sid = package_sid
        return process
    finally:
        os.set_handle_inheritable(handle, False)
        os.set_handle_inheritable(error_handle, False)
        os.set_handle_inheritable(input_handle, False)
        if input_stream is None:
            stdin.close()
        k.DeleteProcThreadAttributeList(buf)
        adv.FreeSid(sid)
        if capabilities:
            capabilities.close()


class RuntimeCapabilities:
    """Fixed capability SIDs for a host-selected runtime policy."""
    def __init__(self, names):
        class Entry(c.Structure):
            _fields_ = [("Sid", c.c_void_p), ("Attributes", w.DWORD)]
        self.allocations = []
        derive = c.WinDLL("kernelbase", use_last_error=True).DeriveCapabilitySidsFromName
        derive.argtypes = [w.LPCWSTR, c.POINTER(c.c_void_p), c.POINTER(w.DWORD), c.POINTER(c.c_void_p), c.POINTER(w.DWORD)]
        entries = []
        try:
            for name in names:
                groups, values = c.c_void_p(), c.c_void_p()
                group_count, count = w.DWORD(), w.DWORD()
                if not derive(name, c.byref(groups), c.byref(group_count), c.byref(values), c.byref(count)):
                    raise c.WinError(c.get_last_error())
                for array, length in ((groups, group_count.value), (values, count.value)):
                    for i in range(length):
                        value = c.cast(array, c.POINTER(c.c_void_p))[i]
                        self.allocations.append(value)
                        if array is values:
                            entries.append(Entry(value, 4))
                    self.allocations.append(array)
            self.entries = (Entry * len(entries))(*entries)
            self.pointer, self.count = c.cast(self.entries, c.c_void_p), len(entries)
        except BaseException:
            self.close()
            raise

    def close(self):
        kernel = c.WinDLL("kernel32")
        kernel.LocalFree.argtypes = [c.c_void_p]
        for value in reversed(self.allocations):
            kernel.LocalFree(value)
        self.allocations.clear()


class MusicCapabilities(RuntimeCapabilities):
    """The two owner-authorized music capabilities, never model input."""
    def __init__(self):
        super().__init__(("registryRead", "privateNetworkClientServer"))


class SpeechCapabilities(RuntimeCapabilities):
    """Offline speech runtime: local registry read only, never network."""
    def __init__(self):
        super().__init__(("registryRead",))

class BASIC_LIMIT(c.Structure):
    _fields_=[("PerProcessUserTimeLimit",c.c_int64),("PerJobUserTimeLimit",c.c_int64),("LimitFlags",w.DWORD),("MinimumWorkingSetSize",c.c_size_t),("MaximumWorkingSetSize",c.c_size_t),("ActiveProcessLimit",w.DWORD),("Affinity",c.c_size_t),("PriorityClass",w.DWORD),("SchedulingClass",w.DWORD)]
class IO_COUNTERS(c.Structure):
    _fields_=[(name,c.c_uint64) for name in ["ReadOperationCount","WriteOperationCount","OtherOperationCount","ReadTransferCount","WriteTransferCount","OtherTransferCount"]]
class EXTENDED_LIMIT(c.Structure):
    _fields_=[("BasicLimitInformation",BASIC_LIMIT),("IoInfo",IO_COUNTERS),("ProcessMemoryLimit",c.c_size_t),("JobMemoryLimit",c.c_size_t),("PeakProcessMemoryUsed",c.c_size_t),("PeakJobMemoryUsed",c.c_size_t)]
class SandboxedProcess:
    def __init__(self,k,handle,job,pid):self.k,self.handle,self.job,self.pid,self.returncode=k,handle,job,pid,None
    def poll(self):
        if self.returncode is not None:return self.returncode
        code=w.DWORD()
        if not self.k.GetExitCodeProcess(self.handle,c.byref(code)):raise c.WinError(c.get_last_error())
        if code.value!=259:
            self.returncode=code.value;self.k.CloseHandle(self.handle);self.handle=None
            self.k.CloseHandle(self.job);self.job=None
        return self.returncode
    def wait(self,timeout=None):
        started=time.monotonic()
        while self.poll() is None:
            if timeout is not None and time.monotonic()-started>timeout:raise subprocess.TimeoutExpired("lpac",timeout)
            time.sleep(.05)
        return self.returncode
    def kill(self):
        if self.job:self.k.TerminateJobObject(self.job,1)
    def terminate(self):
        self.kill()
    def __del__(self):
        if self.job:self.k.CloseHandle(self.job)
        if self.handle:self.k.CloseHandle(self.handle)


def host_pipe_path(local_name, process):
    prefix = "\\\\.\\pipe\\LOCAL\\"
    if not local_name.startswith(prefix) or "\\" in local_name[len(prefix):]:
        raise ValueError("invalid_music_named_pipe")
    session = w.DWORD()
    kernel = c.WinDLL("kernel32", use_last_error=True)
    if not kernel.ProcessIdToSessionId(process.pid, c.byref(session)):
        raise c.WinError(c.get_last_error())
    return "\\\\.\\pipe\\Sessions\\" + str(session.value) + "\\AppContainerNamedObjects\\" + process.package_sid + "\\" + local_name[len(prefix):]
