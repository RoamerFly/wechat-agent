"""Package the approved transparent PNG into Windows ICO size variants.

This performs format conversion/resampling only. The original generated design
is retained alongside the ICO; no image model or API is called by this script.
"""
import argparse
from pathlib import Path

from PIL import Image

SIZES = (16, 20, 24, 32, 40, 48, 64, 96, 128, 256)


def build(source, destination):
    with Image.open(source) as image:
        if image.width != image.height or image.mode != "RGBA":
            raise ValueError("square_rgba_icon_source_required")
        if image.getchannel("A").getextrema()[0] != 0:
            raise ValueError("icon_source_requires_transparent_background")
        destination.parent.mkdir(parents=True, exist_ok=True)
        image.save(destination, format="ICO", sizes=[(size, size) for size in SIZES])
    with Image.open(destination) as icon:
        if icon.ico.sizes() != {(size, size) for size in SIZES}:
            raise ValueError("icon_size_variants_incomplete")
        for size in SIZES:
            frame = icon.ico.getimage((size, size))
            if frame.mode != "RGBA" or frame.getchannel("A").getextrema()[0] != 0:
                raise ValueError("icon_transparency_lost")
    print("Windows ICO verified:", ", ".join(map(str, SIZES)))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    root = Path(__file__).resolve().parents[1] / "desktop/windows/assets"
    parser.add_argument("--source", type=Path, default=root / "agent-home.png")
    parser.add_argument("--output", type=Path, default=root / "agent-home.ico")
    args = parser.parse_args()
    build(args.source, args.output)
