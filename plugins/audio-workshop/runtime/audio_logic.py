"""Bounded PCM WAV preparation primitives for audio-workshop.

The host materializes exactly one approved WAV in a task-scoped directory.
This module never discovers paths, starts programs, or accesses the network.
"""
from __future__ import annotations

import hashlib
import math
from pathlib import Path
import re
import wave


MAX_INPUT_BYTES = 512 * 1024 * 1024
MAX_OUTPUT_BYTES = 512 * 1024 * 1024
MAX_INPUT_FRAMES = 1_000_000_000
CHUNK_FRAMES = 16_384


class AudioError(ValueError):
    pass


def digest(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as reader:
        for data in iter(lambda: reader.read(1024 * 1024), b""):
            hasher.update(data)
    return hasher.hexdigest()


def safe_filename(value: object, extension: str = ".wav") -> str:
    if not isinstance(value, str):
        return "prepared-audio" + extension
    name = value.strip().replace("\\", "_").replace("/", "_")
    name = re.sub(r"[^A-Za-z0-9._()\- \u4e00-\u9fff]", "_", name).strip(". ")
    if not name:
        name = "prepared-audio"
    if not name.lower().endswith(extension):
        name += extension
    return name[:120]


def _spec(path: Path) -> dict:
    try:
        with wave.open(str(path), "rb") as reader:
            if reader.getcomptype() != "NONE":
                raise AudioError("audio_wav_compression_not_supported")
            width = reader.getsampwidth()
            channels = reader.getnchannels()
            rate = reader.getframerate()
            frames = reader.getnframes()
    except AudioError:
        raise
    except (EOFError, wave.Error, OSError):
        raise AudioError("audio_wav_invalid")
    if width not in (1, 2, 3, 4) or channels < 1 or channels > 8 or rate < 1 or rate > 384_000:
        raise AudioError("audio_wav_format_not_supported")
    if frames < 1 or frames > MAX_INPUT_FRAMES:
        raise AudioError("audio_wav_frame_count_invalid")
    _validate_wave_data(path, frames * width * channels)
    return {"sample_width": width, "channels": channels, "sample_rate": rate, "frames": frames}


def _validate_wave_data(path: Path, expected_data_bytes: int) -> None:
    """Reject WAV containers that claim more data than the file contains."""
    try:
        size = path.stat().st_size
        with path.open("rb") as source:
            header = source.read(12)
            if len(header) != 12 or header[:4] != b"RIFF" or header[8:] != b"WAVE":
                raise AudioError("audio_wav_invalid")
            offset, data_bytes = 12, None
            for _ in range(128):
                if offset + 8 > size:
                    break
                source.seek(offset)
                chunk = source.read(8)
                if len(chunk) != 8:
                    break
                declared = int.from_bytes(chunk[4:], "little")
                begin, end = offset + 8, offset + 8 + declared
                if end > size:
                    raise AudioError("audio_wav_truncated")
                if chunk[:4] == b"data":
                    data_bytes = declared
                    break
                offset = end + (declared & 1)
    except AudioError:
        raise
    except OSError:
        raise AudioError("audio_wav_invalid")
    if data_bytes is None:
        raise AudioError("audio_wav_invalid")
    if data_bytes != expected_data_bytes:
        raise AudioError("audio_wav_truncated")


def _sample_to_i16(raw: bytes, width: int) -> int:
    if width == 1:
        return (raw[0] - 128) << 8
    if width == 2:
        return int.from_bytes(raw, "little", signed=True)
    if width == 3:
        value = int.from_bytes(raw + (b"\xff" if raw[2] & 0x80 else b"\0"), "little", signed=True)
        return value >> 8
    return int.from_bytes(raw, "little", signed=True) >> 16


def _pcm16(data: bytes, width: int, input_channels: int, output_channels: int) -> bytes:
    frame_size = width * input_channels
    if len(data) % frame_size:
        raise AudioError("audio_wav_invalid")
    if width == 2 and output_channels == input_channels:
        return data
    result = bytearray()
    for offset in range(0, len(data), frame_size):
        samples = [_sample_to_i16(data[offset + channel * width:offset + (channel + 1) * width], width) for channel in range(input_channels)]
        if output_channels == 1:
            samples = [round(sum(samples) / len(samples))]
        for sample in samples:
            result.extend(max(-32768, min(32767, sample)).to_bytes(2, "little", signed=True))
    return bytes(result)


def prepare(input_path: Path, output_dir: Path, filename: object, start_seconds: object, duration_seconds: object, channels: object) -> dict:
    try:
        size = input_path.stat().st_size
    except OSError:
        raise AudioError("audio_input_missing")
    if size < 44:
        raise AudioError("audio_wav_invalid")
    if size > MAX_INPUT_BYTES:
        raise AudioError("audio_input_too_large")
    if start_seconds is None:
        start_seconds = 0
    if type(start_seconds) not in (int, float) or not math.isfinite(start_seconds) or start_seconds < 0:
        raise AudioError("audio_start_seconds_invalid")
    if duration_seconds is not None and (type(duration_seconds) not in (int, float) or not math.isfinite(duration_seconds) or duration_seconds <= 0):
        raise AudioError("audio_duration_seconds_invalid")
    if channels is None:
        channels = "preserve"
    if channels not in ("preserve", "mono"):
        raise AudioError("audio_channels_invalid")
    spec = _spec(input_path)
    total_seconds = spec["frames"] / spec["sample_rate"]
    if start_seconds >= total_seconds:
        raise AudioError("audio_range_empty")
    start_frame = int(math.floor(start_seconds * spec["sample_rate"]))
    available_frames = spec["frames"] - start_frame
    if duration_seconds is None or duration_seconds >= available_frames / spec["sample_rate"]:
        requested_frames = available_frames
    else:
        requested_frames = int(math.ceil(duration_seconds * spec["sample_rate"]))
    if requested_frames < 1:
        raise AudioError("audio_range_empty")
    output_frames = min(requested_frames, available_frames)
    output_channels = 1 if channels == "mono" else spec["channels"]
    if 44 + output_frames * output_channels * 2 > MAX_OUTPUT_BYTES:
        raise AudioError("audio_output_too_large")
    output_dir.mkdir(parents=True, exist_ok=True)
    target = output_dir / safe_filename(filename)
    try:
        with wave.open(str(input_path), "rb") as source, wave.open(str(target), "wb") as destination:
            source.setpos(start_frame)
            destination.setnchannels(output_channels)
            destination.setsampwidth(2)
            destination.setframerate(spec["sample_rate"])
            left = output_frames
            while left:
                raw = source.readframes(min(left, CHUNK_FRAMES))
                if not raw:
                    raise AudioError("audio_wav_unexpected_eof")
                frames = len(raw) // (spec["sample_width"] * spec["channels"])
                if not frames:
                    raise AudioError("audio_wav_invalid")
                destination.writeframesraw(_pcm16(raw[:frames * spec["sample_width"] * spec["channels"]], spec["sample_width"], spec["channels"], output_channels))
                left -= frames
    except AudioError:
        target.unlink(missing_ok=True)
        raise
    except (OSError, EOFError, wave.Error):
        target.unlink(missing_ok=True)
        raise AudioError("audio_output_failed")
    actual_seconds = output_frames / spec["sample_rate"]
    return {
        "path": str(target),
        "size": target.stat().st_size,
        "sha256": digest(target),
        "coverage": {
            "start_seconds": start_frame / spec["sample_rate"],
            "requested_duration_seconds": duration_seconds,
            "actual_duration_seconds": actual_seconds,
            "start_frame": start_frame,
            "frames": output_frames,
            "eof": start_frame + output_frames == spec["frames"],
        },
        "verification": {"format": "wav", "sample_rate": spec["sample_rate"], "channels": output_channels, "frames": output_frames, "bits_per_sample": 16},
    }
