"""Opt-in native Windows music isolation and named-pipe integration."""
import hashlib
import json
import os
from pathlib import Path
import runpy
import shutil
import subprocess
import sys
import time
import unittest

from managed_music_ipc import AccountIPC
from managed_music_runner import launch_windows
from music_unix_client import open_request


def native_parent_fields():
    if os.name != "nt":
        return {}
    import ctypes
    from ctypes import wintypes
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel.GetCurrentProcess.restype = wintypes.HANDLE
    kernel.GetProcessTimes.argtypes = [wintypes.HANDLE, ctypes.POINTER(wintypes.FILETIME), ctypes.POINTER(wintypes.FILETIME), ctypes.POINTER(wintypes.FILETIME), ctypes.POINTER(wintypes.FILETIME)]
    kernel.GetProcessTimes.restype = wintypes.BOOL
    created, exited, kernel_time, user_time = (wintypes.FILETIME() for _ in range(4))
    if not kernel.GetProcessTimes(kernel.GetCurrentProcess(), ctypes.byref(created), ctypes.byref(exited), ctypes.byref(kernel_time), ctypes.byref(user_time)):
        raise ctypes.WinError(ctypes.get_last_error())
    return {"parent_pid": os.getpid(), "parent_created_filetime": (int(created.dwHighDateTime) << 32) | int(created.dwLowDateTime)}


class MusicBrokerPolicyTests(unittest.TestCase):
    def test_nltk_pronunciation_resources_keep_verified_zip_and_reject_tampering(self):
        import tempfile
        import zipfile
        from unittest.mock import patch
        import install_windows_gpt_nltk as resources
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            cache = base / 'cache'
            cache.mkdir()
            archive = cache / 'synthetic.zip'
            with zipfile.ZipFile(archive, 'w') as source:
                source.writestr('synthetic/data.txt', 'synthetic')
            expected = hashlib.sha256(archive.read_bytes()).hexdigest()
            with patch.dict(resources.PACKAGES, {'corpora/synthetic': expected}, clear=True):
                target = resources.install(base / 'data', cache)
                self.assertEqual((target / 'corpora/synthetic.zip').read_bytes(), archive.read_bytes())
                self.assertEqual((target / 'corpora/synthetic/data.txt').read_text(), 'synthetic')
                resources.install(target, cache)
                (target / 'corpora/synthetic.zip').write_bytes(b'tampered')
                with self.assertRaisesRegex(RuntimeError, 'existing_nltk_archive_requires_review'):
                    resources.install(target, cache)

    def test_registration_backup_is_not_an_active_project_manifest(self):
        import tempfile
        from register_windows_music import publish_json
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'synthetic.json'
            publish_json(path,{'version':1});original=path.read_bytes()
            publish_json(path,{'version':2});publish_json(path,{'version':2})
            self.assertEqual([p.name for p in path.parent.glob('*.json')],['synthetic.json'])
            backups=list((path.parent/'.backups').iterdir())
            self.assertEqual(len(backups),1)
            self.assertEqual(backups[0].read_bytes(),original)

    def test_unknown_policy_shell_and_environment_without_policy_rejected(self):
        import tempfile
        run = runpy.run_path(str(Path(__file__).with_name("windows_agent_host.pyw")))["run"]
        for fields, error in [
            ({"music_policy": "arbitrary"}, "invalid_music_runtime_policy"),
            ({"music_policy": "registry-read-private-network-v1", "command": "unused"}, "invalid_music_runtime_policy"),
            ({"environment": {"PATH": "unused"}}, "music_runtime_environment_requires_authorized_policy"),
        ]:
            with self.subTest(fields=fields), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "request.json"
                path.write_text(json.dumps({"sandbox": True, **fields}), encoding="utf8")
                run(path)
                result = json.loads(path.with_name("result.json").read_text())
                self.assertIn(error, result["error"])
                self.assertEqual(result["exit_code"], -1)

    def test_offline_speech_policy_is_direct_only_and_never_requests_music_network_policy(self):
        import tempfile
        from unittest.mock import patch

        class Child:
            pid = 1
            returncode = 0
            def poll(self): return 0

        run = runpy.run_path(str(Path(__file__).with_name("windows_agent_host.pyw")))["run"]
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            request = base / "request.json"
            request.write_text(json.dumps({
                "sandbox": True, "cwd": str(base), "roots": [str(base)], "read_only": [],
                "argv": [str(base / "trusted-python.exe"), "-c", "print('speech')"], "command": "",
                "music_policy": "registry-read-offline-speech-v1",
                "environment": {"NLTK_DATA": str(base / "nltk"), "WECHAT_WINDOWS_SPEECH_RUNTIME": "1"},
                **native_parent_fields(),
            }), encoding="utf8")
            with patch("windows_lpac.launch", return_value=Child()) as launch:
                run(request)
            result = json.loads(request.with_name("result.json").read_text(encoding="utf8"))
            self.assertEqual(result["exit_code"], 0, result)
            self.assertFalse(launch.call_args.kwargs["music_runtime"])
            self.assertTrue(launch.call_args.kwargs["speech_runtime"])
            self.assertEqual(launch.call_args.kwargs["environment"]["WECHAT_WINDOWS_SPEECH_RUNTIME"], "1")
            # The launch keeps argv in memory, but the durable current-run
            # control record must not retain text that may be spoken aloud.
            scrubbed = request.read_text(encoding="utf8")
            self.assertIn("offline_speech_request_arguments", scrubbed)
            self.assertNotIn("trusted-python", scrubbed)

    @unittest.skipUnless(os.name == "nt", "native parent identity is Windows-only")
    def test_offline_speech_rejects_reused_or_forged_parent_pid(self):
        import tempfile
        from unittest.mock import patch
        run = runpy.run_path(str(Path(__file__).with_name("windows_agent_host.pyw")))["run"]
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            request = base / "request.json"
            identity = native_parent_fields()
            identity["parent_created_filetime"] += 1
            request.write_text(json.dumps({"sandbox": True, "cwd": str(base), "roots": [str(base)], "read_only": [],
                "argv": [str(base / "trusted-python.exe"), "-c", "print('never')"], "command": "",
                "music_policy": "registry-read-offline-speech-v1", **identity}), encoding="utf8")
            with patch("windows_lpac.launch") as launch:
                run(request)
            result = json.loads(request.with_name("result.json").read_text(encoding="utf8"))
            self.assertIn("broker_parent_unavailable", result.get("error", ""))
            launch.assert_not_called()


@unittest.skipUnless(os.name == "nt" and os.environ.get("WECHAT_WINDOWS_MUSIC_TEST_PYTHON"), "opt-in managed Windows Python")
class WindowsMusicTests(unittest.TestCase):
    def setUp(self):
        import uuid
        self.base = Path(__file__).resolve().parents[1] / "temp/windows-music-tests" / uuid.uuid4().hex
        self.public = self.base / "public_workspace"
        self.python = self.public / "runtimes/python"
        shutil.copytree(Path(os.environ["WECHAT_WINDOWS_MUSIC_TEST_PYTHON"]).parent, self.python)
        shutil.copy2(Path(__file__).with_name("windows_music_site.py"), self.python / "windows_music_site.py")
        (self.python / "sitecustomize.py").write_text("from windows_music_site import initialize\ninitialize()\n")
        pth = next(self.python.glob("python*._pth"))
        pth.write_text(pth.read_text().replace("#import site", "import site"))
        # These tests exercise IPC/isolation, without invoking media programs.
        for name in ("ffmpeg.exe", "ffprobe.exe"):
            (self.python / name).write_bytes(b"synthetic-unused-media-resource")
        self.private = self.base / "pri_workspace" / hashlib.sha256(b"synthetic-a").hexdigest()
        self.other = self.private.parent / hashlib.sha256(b"synthetic-b").hexdigest()
        self.private.mkdir(parents=True)
        self.other.mkdir()
        (self.other / "sentinel").write_text("other-account-secret")
        self.previous = dict(os.environ)
        os.environ["WECHAT_WINDOWS_MUSIC_POLICY"] = "registry-read-private-network-v1"
        os.environ["WECHAT_WINDOWS_MUSIC_MEDIA"] = str(self.python)
        self.addCleanup(self.restore)

    def restore(self):
        os.environ.clear()
        os.environ.update(self.previous)

    def start(self, code, private=None):
        private = private or self.private
        child = launch_windows(self.public, private, private,
                               [str(self.python / "python.exe"), "-X", "utf8", "-c", code], gpu=True)
        self.addCleanup(lambda: child.kill() if child.poll() is None else None)
        return child

    def test_named_pipe_http_account_isolation_and_duplicate_lease(self):
        with AccountIPC(self.base / "control", self.public, self.private) as lease:
            with self.assertRaisesRegex(RuntimeError, "already_running"):
                AccountIPC(self.base / "control", self.public, self.private)
            code = '''import asyncio,pathlib,sys
class Protocol(asyncio.Protocol):
 def connection_made(self,t): self.transport=t;self.data=b''
 def data_received(self,data):
  self.data+=data
  if b'\\r\\n\\r\\n' in self.data:
   if b'/stall ' in self.data: return
   body=b'{"isolated":true}'
   self.transport.write(b'HTTP/1.1 200 OK\\r\\nContent-Length: '+str(len(body)).encode()+b'\\r\\nConnection: close\\r\\n\\r\\n'+body)
   self.transport.close()
async def main():
 servers=await asyncio.get_running_loop().start_serving_pipe(Protocol,sys.argv[1])
 pathlib.Path('ready').write_text('ok')
 await asyncio.Event().wait()
asyncio.run(main())
'''
            # Fixed code embeds only a synthetic pipe name, never input secrets.
            child = self.start("import sys;sys.argv.append(" + repr(lease.socket) + ");" + code)
            for _ in range(100):
                if (self.private / "ready").exists() or child.poll() is not None:
                    break
                time.sleep(.1)
            self.assertIsNone(child.poll(), "isolated named-pipe server failed; inspect account runtime logs")
            from windows_lpac import host_pipe_path
            host_pipe = host_pipe_path(lease.socket, child)
            with open_request(host_pipe, "http://127.0.0.1:8188/system_stats", timeout=5) as response:
                self.assertEqual(json.load(response), {"isolated": True})
            with self.assertRaisesRegex(ValueError, "unexpected_destination"):
                open_request(host_pipe, "http://127.0.0.1:8081/api/status")
            started = time.monotonic()
            with self.assertRaises(TimeoutError):
                open_request(host_pipe, "http://127.0.0.1:8188/stall", timeout=.2)
            self.assertLess(time.monotonic() - started, 2)
            probe = "import os,sys\ntry:\n os.open(" + repr(host_pipe) + ",os.O_RDWR)\nexcept OSError: pass\nelse: sys.exit('foreign account pipe opened')"
            other = self.start(probe, self.other)
            self.assertEqual(other.wait(10), 0, "another account reached the pipe")
            child.kill()
            self.assertNotEqual(child.wait(10), 0)

    def test_files_credentials_policy_and_no_implicit_music_privilege(self):
        os.environ["MANAGED_SYNTHETIC_SECRET"] = "must-not-inherit"
        code = "import pathlib,os\nassert not os.environ.get('MANAGED_SYNTHETIC_SECRET')\n"
        code += "for p in " + repr([str(self.other / "sentinel"), str(self.base / "outside")]) + ":\n try:\n  pathlib.Path(p).write_text('leak')\n except PermissionError: pass\n else: raise RuntimeError('outside write')\n"
        code += "try:\n pathlib.Path(" + repr(str(self.other / "sentinel")) + ").read_text()\nexcept PermissionError:pass\nelse:raise RuntimeError('outside read')\n"
        code += "pathlib.Path('inside').write_text('ok')"
        self.assertEqual(self.start(code).wait(15), 0)
        self.assertEqual((self.private / "inside").read_text(), "ok")
        os.environ.pop("WECHAT_WINDOWS_MUSIC_POLICY")
        with self.assertRaisesRegex(RuntimeError, "not_authorized"):
            self.start("pass")

    def test_multiprocessing_pipe_uses_account_local_namespace(self):
        code = "from multiprocessing import Pipe;import multiprocessing.connection as c;assert c.arbitrary_address('AF_PIPE').startswith('\\\\\\\\.\\\\pipe\\\\LOCAL\\\\');a,b=Pipe(False);b.send_bytes(b'synthetic');assert a.recv_bytes()==b'synthetic';a.close();b.close()"
        self.assertEqual(self.start(code).wait(15), 0)


if __name__ == "__main__":
    unittest.main()
