"""Synthetic process acceptance for the station-only RVC bridge.

This test starts the real HTTP broker with a temporary, scoped station home.
It replaces only the local model conversion with a valid tiny PCM WAV writer:
authentication, scope checks, durable request lookup, the job journal and the
HTTP handlers remain the production code.  It never starts Comfy/YuE, opens a
real voice model, or contacts WeChat.
"""

import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.error
import urllib.parse
import urllib.request
import wave

sys.path.insert(0, str(Path(__file__).resolve().parent))
from process_window import no_console_options


ROOT = Path(__file__).resolve().parents[1]
SCRIPTS = ROOT / "scripts"
FISH = "fish-" + "a" * 48
FISH_HASH = hashlib.sha256(FISH.encode("utf-8")).hexdigest()
TOKEN = "station-rvc-synthetic-token"
REQUEST_KEY = "b" * 64


def pcm_wav_bytes(seconds=0.25, rate=8000):
    """Return a small valid mono PCM16 WAV without depending on ffmpeg."""
    with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as file:
        path = Path(file.name)
    try:
        with wave.open(str(path), "wb") as audio:
            audio.setnchannels(1)
            audio.setsampwidth(2)
            audio.setframerate(rate)
            audio.writeframes(b"\0\0" * round(seconds * rate))
        return path.read_bytes()
    finally:
        path.unlink(missing_ok=True)


class StationRVCBridgeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.public = self.root / "public_workspace"
        self.private = self.root / "pri_workspace" / FISH_HASH
        self.state = self.private / "music" / "jobs"
        self.public.mkdir(parents=True)
        self.state.mkdir(parents=True)
        # This deliberately sits outside both managed mounts *and* their
        # common workspace parent.  The station binder rejects a token that a
        # tool running in the workspace could traverse to.
        control = tempfile.TemporaryDirectory(prefix="station-rvc-control-")
        self.addCleanup(control.cleanup)
        token_root = Path(control.name).resolve()
        self.token_file = token_root / "token"
        self.ready_root = token_root / "station-rvc-ready"
        self.ready_root.mkdir()
        self.token_file.write_text(TOKEN, encoding="utf-8")
        self.counter = self.root / "conversion-count"
        self.wav = pcm_wav_bytes()
        self.wav_sha256 = hashlib.sha256(self.wav).hexdigest()
        self.children = []
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.addCleanup(self.stop_all)

    def stop_all(self):
        for child in self.children:
            if child.poll() is None:
                child.terminate()
        for child in self.children:
            try:
                child.wait(timeout=10)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait(timeout=10)
            for stream in (child.stdin, child.stderr):
                if stream is not None:
                    stream.close()

    def free_port(self):
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.bind(("127.0.0.1", 0))
            return sock.getsockname()[1]

    def bootstrap(self):
        """A child-only stub for model work, retaining bridge HTTP/journaling."""
        path = self.root / "station-rvc-bootstrap.py"
        path.write_text(
            """
import hashlib, json, os, sys, wave
from pathlib import Path
sys.path.insert(0, os.environ['STATION_RVC_SCRIPTS'])
import station_rvc
import yue_bridge

PROFILE = {
    'profile': 'private-test', 'voice_profile_id': 'private-test',
    'engine': 'rvc', 'model_sha256': '1' * 64, 'index_sha256': '2' * 64,
    'requested_device': 'cpu', 'rvc_device': 'cpu', 'config_sha256': '3' * 64,
}
def checked_profile(public, private, body, profile_reader=None):
    identity = body.get('voice_profile_id')
    if not isinstance(identity, str) or not identity:
        raise ValueError('rvc_voice_profile_required')
    if identity != 'private-test':
        raise ValueError('rvc_voice_profile_not_registered')
    return 'synthetic-private-test.json', dict(PROFILE)
station_rvc.checked_profile = checked_profile

def synthetic_convert(body, state, jid, *unused, **ignored):
    state = Path(state)
    source = state / (jid + '-source.audio')
    if not source.is_file():
        raise ValueError('synthetic_source_missing')
    counter = Path(os.environ['STATION_RVC_COUNTER'])
    with counter.open('a', encoding='utf-8') as file:
        file.write('1')
    if body.get('title') == 'waiting cancellation':
        release = state / 'synthetic-release'
        while not release.exists():
            import time; time.sleep(.02)
    output = state / (jid + '-rvc.wav')
    with wave.open(str(output), 'wb') as audio:
        audio.setnchannels(1); audio.setsampwidth(2); audio.setframerate(8000)
        audio.writeframes(b'\\0\\0' * 2000)
    metadata = dict(PROFILE, duration_seconds=.25)
    # Keep both conventional sidecar spellings for an explicit verifier while
    # the bridge owns which one it accepts as canonical.
    for sidecar in (Path(str(output) + '.voice.json'), output.with_suffix('.voice.json')):
        sidecar.write_text(json.dumps(metadata), encoding='utf-8')
    return {
        'file': output.name, 'bytes': output.stat().st_size,
        'duration_seconds': .25, 'requested_seconds': .25,
        'output_sha256': hashlib.sha256(output.read_bytes()).hexdigest(),
    }
yue_bridge.rvc_cover.convert = synthetic_convert
sys.argv[0] = str(Path(yue_bridge.__file__))
yue_bridge.main()
""".lstrip(),
            encoding="utf-8",
        )
        return path

    def start(self, desktop=False):
        port = self.free_port()
        nonce = secrets.token_hex(32)
        ready_file = self.ready_root / f"{FISH_HASH}-{nonce}.json"
        env = dict(
            os.environ,
            PYTHONDONTWRITEBYTECODE="1",
            STATION_RVC_SCRIPTS=os.environ.get("AHS_STATION_RVC_CONTROL_ROOT", str(SCRIPTS)),
            STATION_RVC_COUNTER=str(self.counter),
        )
        command = [
                sys.executable, "-u", str(self.bootstrap()),
                "--rvc-only", "--station-fish", FISH,
                "--project", str(self.public), "--state", str(self.state),
                "--managed-public", str(self.public),
                "--managed-private", str(self.private),
                "--token-file", str(self.token_file), "--port", str(port),
                "--ready-file", str(ready_file), "--ready-nonce", nonce,
        ]
        if desktop:
            command.append("--desktop")
        child = subprocess.Popen(
            command,
            cwd=str(ROOT), env=env, stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE, **no_console_options(),
        )
        self.children.append(child)
        base = f"http://127.0.0.1:{port}"
        self.wait_for(ready_file.is_file, child)
        ready = json.loads(ready_file.read_text(encoding="utf-8"))
        self.assertEqual(ready, {"nonce": nonce, "port": port, "scope": FISH_HASH, "pid": child.pid})
        self.wait_for(lambda: self.request(base, "/health")[0] == 200, child)
        return child, base

    def request(self, base, path, method="GET", body=None, authenticated=True,
                fish=FISH_HASH, key=None):
        headers = {"X-Agent-Station-Fish": fish}
        if authenticated:
            headers["Authorization"] = "Bearer " + TOKEN
        if key is not None:
            headers["Idempotency-Key"] = key
        data = None
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(base + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=3) as response:
                raw = response.read()
                try:
                    value = json.loads(raw) if raw else {}
                except (UnicodeDecodeError, json.JSONDecodeError):
                    value = {}
                return response.status, value, dict(response.headers), raw
        except urllib.error.HTTPError as error:
            raw = error.read()
            try:
                value = json.loads(raw)
            except json.JSONDecodeError:
                value = {"raw": raw.decode("utf-8", "replace")}
            return error.code, value, dict(error.headers), raw

    def wait_for(self, predicate, child=None):
        deadline = time.monotonic() + 12
        while time.monotonic() < deadline:
            if child is not None and child.poll() is not None:
                details = child.stderr.read().decode("utf-8", "replace") if child.stderr else ""
                self.fail("station RVC child stopped before becoming ready: " + details[-1000:])
            try:
                if predicate():
                    return
            except (OSError, ValueError, urllib.error.URLError):
                pass
            time.sleep(.04)
        self.fail("timed out waiting for station RVC bridge")

    def body(self, profile=True, title="synthetic station RVC"):
        value = {
            "title": title,
            "reference_base64": base64.b64encode(self.wav).decode("ascii"),
            "reference_source_sha256": self.wav_sha256,
            "reference_start_seconds": 0,
            "reference_seconds": .25,
            "reference_pretrimmed": True,
        }
        if profile:
            value["voice_profile_id"] = "private-test"
            value["profile_config_sha256"] = "3" * 64
        return value

    def test_station_rvc_http_scope_profile_idempotency_and_restart(self):
        child, base = self.start()

        status, _, _, _ = self.request(base, "/health", authenticated=False)
        self.assertEqual(status, 401)
        foreign = hashlib.sha256(("fish-" + "b" * 48).encode("utf-8")).hexdigest()
        status, _, _, _ = self.request(base, "/health", fish=foreign)
        self.assertEqual(status, 409)

        status, health, _, _ = self.request(base, "/health")
        self.assertEqual(status, 200)
        self.assertTrue(health.get("ready"))
        self.assertEqual(health.get("engine"), "rvc")
        self.assertEqual(health.get("mode"), "station_rvc")
        # Broker readiness and named-profile readiness are intentionally not
        # the same check.
        status, profile_health, _, _ = self.request(
            base, "/rvc-health?" + urllib.parse.urlencode({"voice_profile_id": "private-test"})
        )
        self.assertEqual(status, 200)
        self.assertTrue(profile_health.get("ready"))
        self.assertEqual(profile_health.get("engine"), "rvc")
        self.assertEqual(profile_health.get("profile"), "private-test")
        self.assertEqual(profile_health.get("voice_profile_id"), "private-test")
        status, absent_profile, _, _ = self.request(
            base, "/rvc-health?" + urllib.parse.urlencode({"voice_profile_id": "not-registered"})
        )
        self.assertTrue(status >= 400 or not absent_profile.get("ready"))

        status, missing, _, _ = self.request(base, "/rvc-jobs", "POST", self.body(profile=False), key="c" * 64)
        self.assertGreaterEqual(status, 400)
        self.assertIn("profile", missing.get("error", ""))
        wrong_pin = dict(self.body(), profile_config_sha256="4" * 64)
        status, error, _, _ = self.request(base, "/rvc-jobs", "POST", wrong_pin, key="d" * 64)
        self.assertEqual((status, error.get("error")), (409, "station_rvc_profile_changed"))

        status, created, _, _ = self.request(base, "/rvc-jobs", "POST", self.body(), key=REQUEST_KEY)
        self.assertIn(status, (200, 202))
        job_id = created["id"]
        self.assertRegex(job_id, r"^[a-f0-9]{32}$")
        status, duplicate, _, _ = self.request(base, "/rvc-jobs", "POST", self.body(), key=REQUEST_KEY)
        self.assertEqual(status, 200)
        self.assertEqual(duplicate["id"], job_id)
        self.wait_for(lambda: self.request(base, "/jobs/" + job_id)[1].get("state") == "completed", child)
        status, job, _, _ = self.request(base, "/jobs/" + job_id)
        self.assertEqual(status, 200)
        self.assertEqual(job.get("sha256"), self.wav_sha256)
        status, _, _, audio = self.request(base, "/jobs/" + job_id + "/audio")
        self.assertEqual(status, 200)
        self.assertEqual(audio, self.wav)
        self.assertEqual(hashlib.sha256(audio).hexdigest(), self.wav_sha256)
        self.assertEqual(self.counter.read_text(encoding="utf-8"), "1")

        child.terminate()
        child.wait(timeout=10)
        child, restarted = self.start()
        # This is deliberately a lookup only: it proves the request journal
        # resolves after restart without submitting a second conversion.
        status, request_job, _, _ = self.request(restarted, "/rvc-requests/" + REQUEST_KEY)
        self.assertEqual(status, 200)
        self.assertEqual(request_job["id"], job_id)
        self.assertEqual(self.counter.read_text(encoding="utf-8"), "1")

        # A conversion that has actually entered the local worker remains
        # cancellable.  Completion after the release signal must not revive it
        # or publish an audio artefact.
        cancel_key = "d" * 64
        status, waiting, _, _ = self.request(
            restarted, "/rvc-jobs", "POST", self.body(title="waiting cancellation"), key=cancel_key
        )
        self.assertIn(status, (200, 202))
        cancel_id = waiting["id"]
        self.wait_for(lambda: self.counter.exists() and self.counter.read_text(encoding="utf-8") == "11", child)
        status, cancelling, _, _ = self.request(restarted, "/jobs/" + cancel_id + "/cancel", "POST", {})
        self.assertEqual(status, 200)
        self.assertIn(cancelling.get("state"), ("cancelling", "cancelled"))
        (self.state / "synthetic-release").touch()
        self.wait_for(lambda: self.request(restarted, "/jobs/" + cancel_id)[1].get("state") == "cancelled", child)
        status, _, _, _ = self.request(restarted, "/jobs/" + cancel_id + "/audio")
        self.assertEqual(status, 409)

        # The optional desktop control pipe must stop the bridge without a
        # console window or a process kill.  This runs only after all journal
        # assertions above, because one state directory has one live broker.
        child.terminate()
        child.wait(timeout=10)
        desktop_child, desktop = self.start(desktop=True)
        self.assertEqual(self.request(desktop, "/health")[0], 200)
        self.assertIsNotNone(desktop_child.stdin)
        desktop_child.stdin.write(b"stop\n")
        desktop_child.stdin.flush()
        self.assertEqual(desktop_child.wait(timeout=10), 0)


if __name__ == "__main__":
    unittest.main()
