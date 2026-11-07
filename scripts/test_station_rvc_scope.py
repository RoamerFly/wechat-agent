"""Focused, filesystem-only checks for the Station RVC trust boundary."""

import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
import wave

sys.path.insert(0, str(Path(__file__).parent))
import station_rvc


FISH = "fish-" + "a" * 48


class StationRVCScopeTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.public = self.root / "public_workspace"
        self.private = self.root / "pri_workspace" / hashlib.sha256(FISH.encode()).hexdigest()
        self.public.mkdir()
        self.private.mkdir(parents=True)
        descriptor, token_name = tempfile.mkstemp(prefix="station-rvc-token-")
        os.close(descriptor)
        self.token = Path(token_name)
        self.token.write_text("synthetic-token", encoding="ascii")
        self.addCleanup(self.token.unlink, missing_ok=True)

    def touch(self, path, data=b"x"):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        return path

    def profile_fixture(self):
        runtime = self.touch(self.public / "runtimes" / "music.exe")
        self.touch(self.public / "runtimes" / "environment" / "ready")
        self.touch(self.public / "models" / "separator" / "ready")
        project = self.public / "projects" / "rvc-upstream"
        self.touch(project / "assets" / "hubert_base" / "pytorch_model.bin")
        self.touch(project / "assets" / "rmvpe" / "rmvpe.pt")
        self.touch(project / "infer" / "vc" / "modules.py")
        model = self.touch(self.public / "models" / "voice-library" / "test.pth", b"model")
        index = self.touch(self.public / "models" / "voice-library" / "test.index", b"index")
        config = self.public / "models" / "voice-library" / "test.json"
        item = {
            "enabled": True, "profile": "safe-label", "engine": "rvc",
            "python": str(runtime), "rvc_python": str(runtime),
            "runtime": str(self.public / "runtimes" / "environment"),
            "rvc_project": str(project), "rvc_model": str(model), "rvc_index": str(index),
            "rvc_model_sha256": hashlib.sha256(model.read_bytes()).hexdigest(),
            "rvc_index_sha256": hashlib.sha256(index.read_bytes()).hexdigest(),
            "rvc_device": "cpu",
        }
        config.write_text(json.dumps(item), encoding="utf-8")
        manifest = self.public / "manifests" / "public-test.json"
        manifest.parent.mkdir(parents=True, exist_ok=True)
        manifest.write_text(json.dumps({"configuration": str(config)}), encoding="utf-8")
        return config

    def test_bind_and_profile_keep_identity_and_label_distinct(self):
        public, private, state, token, scope = station_rvc.bind_scope(
            FISH, self.public, self.private, self.private / "music" / "jobs", self.token
        )
        self.assertEqual(scope, hashlib.sha256(FISH.encode()).hexdigest())
        self.assertEqual(token, self.token.resolve())
        config = self.profile_fixture()
        seen = []

        def reader(path, expected=None):
            seen.append(path)
            self.assertEqual(expected, hashlib.sha256(config.read_bytes()).hexdigest())
            return {"profile": "safe-label", "engine": "rvc", "config_sha256": hashlib.sha256(config.read_bytes()).hexdigest()}

        got, profile = station_rvc.checked_profile(public, private, {"voice_profile_id": "public-test"}, reader)
        self.assertEqual(Path(got), config.resolve())
        self.assertEqual(profile["profile"], "safe-label")
        self.assertEqual(len(seen), 1)
        self.assertTrue(state.is_dir())

    def test_read_rejects_tampered_registered_result(self):
        _, _, state, _, _ = station_rvc.bind_scope(
            FISH, self.public, self.private, self.private / "music" / "jobs", self.token
        )
        jid = "b" * 32
        wav = state / (jid + "-rvc.wav")
        with wave.open(str(wav), "wb") as output:
            output.setnchannels(1)
            output.setsampwidth(2)
            output.setframerate(8000)
            output.writeframes(b"\0\0" * 2000)
        model_hash, index_hash = "1" * 64, "2" * 64
        (state / (jid + "-rvc.voice.json")).write_text(json.dumps({
            "profile": "safe-label", "engine": "RVC", "model_sha256": model_hash,
            "index_sha256": index_hash, "requested_device": "cpu", "rvc_device": "cpu",
            "duration_seconds": .25,
        }), encoding="utf-8")
        profile = {"profile": "safe-label", "model_sha256": model_hash,
                   "index_sha256": index_hash, "requested_device": "cpu"}
        output = {"file": wav.name, "bytes": wav.stat().st_size,
                  "duration_seconds": .25, "requested_seconds": .25}
        verified = station_rvc.verify_output(state, jid, output, profile)
        job = {"id": jid, "singing_voice": profile, **verified}
        self.assertEqual(station_rvc.read_output(state, jid, job), wav.read_bytes())
        job["sha256"] = "0" * 64
        with self.assertRaises(ValueError):
            station_rvc.read_output(state, jid, job)


if __name__ == "__main__":
    unittest.main()
