"""Offline contract tests for audio-workshop. Uses no real recordings."""
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
import wave

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "runtime"))
import audio_logic
import main


def write_pcm(path: Path, width: int, channels: int, rate: int, frames: list[tuple[int, ...]]):
    data = bytearray()
    for frame in frames:
        for sample in frame:
            if width == 1:
                data.append(max(0, min(255, sample + 128)))
            elif width == 2:
                data.extend(sample.to_bytes(2, "little", signed=True))
            elif width == 3:
                data.extend((sample & 0xffffff).to_bytes(3, "little", signed=False))
            else:
                data.extend(sample.to_bytes(4, "little", signed=True))
    with wave.open(str(path), "wb") as target:
        target.setnchannels(channels)
        target.setsampwidth(width)
        target.setframerate(rate)
        target.writeframes(bytes(data))


class AudioWorkshopTests(unittest.TestCase):
    def envelope(self, root: Path, source: Path):
        return {"station/audio": {
            "input_root": str(root), "output_root": str(root / "out"), "output_dir": str(root / "out"),
            "input": {"artifact_id": "artifact-safe", "path": str(source), "sha256": hashlib.sha256(source.read_bytes()).hexdigest(), "mime_type": "audio/wav"},
        }}

    def test_prepare_clips_frames_and_preserves_stereo(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            source = root / "source.wav"
            write_pcm(source, 2, 2, 10, [(1000, -1000), (2000, -2000), (3000, -3000), (4000, -4000), (5000, -5000)])
            result = main.call("audio.prepare", {"artifact": {"artifact_id": "artifact-safe", "sha256": hashlib.sha256(source.read_bytes()).hexdigest()}, "start_seconds": 0.1, "duration_seconds": 0.25, "filename": "cut.wav"}, self.envelope(root, source))
            self.assertTrue(result["ok"])
            self.assertEqual(result["coverage"], {"start_seconds": 0.1, "requested_duration_seconds": 0.25, "actual_duration_seconds": 0.3, "start_frame": 1, "frames": 3, "eof": False})
            item = result["outputs"][0]
            self.assertEqual(item["verification"], {"format": "wav", "sample_rate": 10, "channels": 2, "frames": 3, "bits_per_sample": 16})
            with wave.open(item["path"], "rb") as output:
                self.assertEqual(output.getparams()[:4], (2, 2, 10, 3))
                self.assertEqual(output.readframes(3), (2000).to_bytes(2, "little", signed=True) + (-2000).to_bytes(2, "little", signed=True) + (3000).to_bytes(2, "little", signed=True) + (-3000).to_bytes(2, "little", signed=True) + (4000).to_bytes(2, "little", signed=True) + (-4000).to_bytes(2, "little", signed=True))

    def test_prepare_mono_converts_8_24_and_32_bit_pcm(self):
        for width, source_frames, expected in (
            (1, [(10, -10), (20, -20)], [0, 0]),
            (3, [(0x200000, -0x200000)], [0]),
            (4, [(0x20000000, -0x20000000)], [0]),
        ):
            with self.subTest(width=width), tempfile.TemporaryDirectory() as raw:
                root = Path(raw)
                source = root / "source.wav"
                write_pcm(source, width, 2, 4, source_frames)
                result = main.call("audio.prepare", {"channels": "mono"}, self.envelope(root, source))
                item = result["outputs"][0]
                with wave.open(item["path"], "rb") as output:
                    self.assertEqual((output.getsampwidth(), output.getnchannels(), output.getframerate()), (2, 1, 4))
                    values = [int.from_bytes(output.readframes(1), "little", signed=True) for _ in expected]
                    self.assertEqual(values, expected)

    def test_pcm16_preserve_fast_path_keeps_input_bytes(self):
        raw = b"\x01\x80\xff\x7f\x00\x00\x34\x12"
        self.assertIs(audio_logic._pcm16(raw, 2, 2, 2), raw)

    def test_rejects_raw_paths_hash_mismatch_and_unsupported_formats(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            source = root / "source.wav"
            write_pcm(source, 2, 1, 4, [(1,), (2,)])
            meta = self.envelope(root, source)
            with self.assertRaises(audio_logic.AudioError) as error:
                main.call("audio.prepare", {"path": str(source)}, meta)
            self.assertEqual(str(error.exception), "audio_arguments_invalid")
            meta["station/audio"]["input"]["sha256"] = "0" * 64
            with self.assertRaises(audio_logic.AudioError) as error:
                main.call("audio.prepare", {}, meta)
            self.assertEqual(str(error.exception), "audio_input_hash_mismatch")
            meta = self.envelope(root, source)
            meta["station/audio"]["input"]["mime_type"] = "audio/mpeg"
            with self.assertRaises(audio_logic.AudioError) as error:
                main.call("audio.prepare", {}, meta)
            self.assertEqual(str(error.exception), "audio_pcm_wav_required")

    def test_range_and_argument_errors_are_explicit(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            source = root / "source.wav"
            write_pcm(source, 2, 1, 4, [(1,), (2,)])
            for arguments, code in (({"start_seconds": 1}, "audio_range_empty"), ({"duration_seconds": 0}, "audio_duration_seconds_invalid"), ({"channels": "surround"}, "audio_channels_invalid")):
                with self.subTest(arguments=arguments), self.assertRaises(audio_logic.AudioError) as error:
                    main.call("audio.prepare", arguments, self.envelope(root, source))
                self.assertEqual(str(error.exception), code)

    def test_huge_finite_ranges_and_output_limit_are_bounded(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            source = root / "source.wav"
            write_pcm(source, 2, 1, 4, [(1,), (2,), (3,), (4,)])
            complete = main.call("audio.prepare", {"duration_seconds": 1e300}, self.envelope(root, source))
            self.assertEqual(complete["coverage"]["frames"], 4)
            self.assertTrue(complete["coverage"]["eof"])
            with self.assertRaises(audio_logic.AudioError) as error:
                main.call("audio.prepare", {"start_seconds": 1e300}, self.envelope(root, source))
            self.assertEqual(str(error.exception), "audio_range_empty")
            with patch.object(audio_logic, "MAX_OUTPUT_BYTES", 47), self.assertRaises(audio_logic.AudioError) as error:
                main.call("audio.prepare", {}, self.envelope(root, source))
            self.assertEqual(str(error.exception), "audio_output_too_large")

    def test_truncated_wav_is_rejected_before_creating_partial_output(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            source = root / "truncated.wav"
            write_pcm(source, 2, 1, 4, [(1,), (2,), (3,)])
            original_meta = self.envelope(root, source)
            source.write_bytes(source.read_bytes()[:-2])
            with self.assertRaises(audio_logic.AudioError) as error:
                main.call("audio.prepare", {"duration_seconds": 0.25}, original_meta)
            self.assertEqual(str(error.exception), "audio_input_hash_mismatch")
            # Rebind the fixture hash as a host would after materializing this
            # file, then assert the WAV container guard is still fail-closed.
            meta = self.envelope(root, source)
            with self.assertRaises(audio_logic.AudioError) as error:
                main.call("audio.prepare", {"duration_seconds": 0.25}, meta)
            self.assertEqual(str(error.exception), "audio_wav_truncated")
            self.assertFalse((root / "out" / "prepared-audio.wav").exists())

    def test_wire_returns_stable_error_payload(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            source = root / "source.wav"
            write_pcm(source, 2, 1, 4, [(1,), (2,)])
            self.assertEqual(main.call("audio.prepare", {}, self.envelope(root, source))["summary"], "audio_prepared")
            self.assertEqual(main.TOOLS[0]["name"], "audio.prepare")


if __name__ == "__main__":
    unittest.main()
