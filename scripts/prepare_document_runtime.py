"""Stage the portable Python-native document runtime from reviewed inputs.

No office suite or Poppler bundle is copied or invoked. Build-only runtime
trees are ignored artifacts, never source-controlled files.
"""
from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import stat
import sys
import uuid
import zipfile

REPORTLAB_OFFLINE_UTILS_SHA256 = "f01013ebd9008d7e1e72ac62d58c6e4bbcd2abc96a72da5371bff70ac82eaf47"


def reject_links(path: Path) -> None:
    for current in [path, *path.parents]:
        try:
            info = current.lstat()
        except FileNotFoundError:
            continue
        if stat.S_ISLNK(info.st_mode) or getattr(info, "st_file_attributes", 0) & stat.FILE_ATTRIBUTE_REPARSE_POINT:
            raise RuntimeError("document runtime contains a linked path")


def digest(path: Path) -> str:
    with path.open("rb") as handle:
        return hashlib.file_digest(handle, "sha256").hexdigest()


def checked_source(value: str | None, label: str, required: bool = False) -> Path | None:
    if not value:
        if required:
            raise RuntimeError(f"{label} is required")
        return None
    source = Path(value).expanduser().resolve(strict=True)
    if not source.is_dir():
        raise RuntimeError(f"{label} must be a directory")
    return source


def copy_tree(source: Path, target: Path, *, python_runtime: bool = False) -> None:
    ignored = ["*.pyc", "__pycache__", "*.tmp", "*.lock"]
    if python_runtime:
        # Native wheels cannot load inside the no-capability LPAC profile. The
        # document fallback deliberately retains only fixed pure-Python code.
        ignored += ["PIL", "Pillow-*.dist-info", "pymupdf", "pymupdf-*.dist-info", "fitz"]
    shutil.copytree(source, target, symlinks=False, ignore=shutil.ignore_patterns(*ignored))


def validate_python(source: Path) -> None:
    if not (source / "reportlab").is_dir() or not (source / "pypdf").is_dir():
        raise RuntimeError("Python packages source must include fixed ReportLab and pypdf")
    version = (source / "reportlab" / "__init__.py").read_text(encoding="utf-8")
    if "Version = \"4.4.9\"" not in version:
        raise RuntimeError("only the reviewed ReportLab 4.4.9 offline patch is supported")
    if not (source / "pypdf-6.21.4.dist-info").is_dir():
        raise RuntimeError("only the reviewed pure-Python pypdf 6.21.4 is supported")


def patch_reportlab_offline(packages: Path) -> str:
    """Apply a narrow audited patch; no URL path is enabled by this runtime."""
    utils = packages / "reportlab" / "lib" / "utils.py"
    if digest(utils) == REPORTLAB_OFFLINE_UTILS_SHA256:
        return REPORTLAB_OFFLINE_UTILS_SHA256
    text = utils.read_text(encoding="utf-8")
    old_pillow = "from PIL import Image"
    new_pillow = "try:\n    from PIL import Image\nexcept ImportError:\n    # LPAC text/table export does not require Pillow's native extension.\n    Image = None"
    old_url = "from urllib.parse import unquote, urlparse\nfrom urllib.request import urlopen\ndef rlUrlRead(name):\n    return urlopen(name).read()"
    new_url = "from urllib.parse import unquote, urlparse\ndef rlUrlRead(name):\n    # URL inputs are rejected by the plugin; retain this helper only for\n    # ReportLab compatibility and import networking code on explicit use.\n    from urllib.request import urlopen\n    return urlopen(name).read()"
    if old_pillow not in text or old_url not in text:
        raise RuntimeError("ReportLab 4.4.9 offline patch anchors do not match")
    text = text.replace(old_pillow, new_pillow, 1).replace(old_url, new_url, 1)
    if hashlib.sha256(text.encode("utf-8")).hexdigest() != REPORTLAB_OFFLINE_UTILS_SHA256:
        raise RuntimeError("ReportLab offline patch differs from the reviewed source")
    utils.write_text(text, encoding="utf-8", newline="\n")
    return digest(utils)


def verify_runtime(root: Path) -> dict:
    manifest = root / "runtime-manifest.json"
    if manifest.is_symlink() or not manifest.is_file():
        raise RuntimeError("existing document runtime has no regular manifest; it was not overwritten")
    record = json.loads(manifest.read_text(encoding="utf-8"))
    components = record.get("components", {})
    if (record.get("schema_version") != 2 or record.get("platform") != ("windows-x64" if sys.platform == "win32" else sys.platform)
            or record.get("policy") != "reportlab-offline-pypdf-reviewed-sources-v1" or components.get("embeddable_ttf") is not True
            or components.get("reportlab") != "4.4.9-offline-patch-v1" or components.get("pypdf") != "6.21.4"
            or components.get("reportlab_utils_sha256") != REPORTLAB_OFFLINE_UTILS_SHA256):
        raise RuntimeError("existing document runtime does not match the reviewed version")
    files = record.get("files")
    if not isinstance(files, dict) or not files or files.get("python-packages/reportlab/lib/utils.py") != REPORTLAB_OFFLINE_UTILS_SHA256:
        raise RuntimeError("existing document runtime has an invalid file manifest")
    for name, expected in files.items():
        relative = Path(name)
        if (not name or relative.anchor or "\\" in name or ":" in name or relative.as_posix() != name
                or ".." in relative.parts or any(part.rstrip(" .") != part for part in relative.parts)):
            raise RuntimeError("document runtime manifest contains an invalid relative path")
        path = root / relative
        reject_links(path)
        if not path.is_file() or digest(path) != expected:
            raise RuntimeError("existing document runtime failed file integrity verification")
    return record


def validate_fonts(source: Path) -> None:
    if not list(source.glob("*.ttf")):
        raise RuntimeError("fonts source needs an embeddable .ttf; CFF/OpenType alone is unsupported")
    if not (list(source.glob("LICENSE*")) + list(source.glob("OFL*"))):
        raise RuntimeError("redistributable font license material is required")


def validate_tesseract(source: Path) -> None:
    executable = source / ("tesseract.exe" if sys.platform == "win32" else "tesseract")
    if not executable.is_file() or not (source / "tessdata" / "chi_sim.traineddata").is_file():
        raise RuntimeError("Tesseract source must contain its executable and tessdata/chi_sim.traineddata")


PDFIUM_WHEEL_SHA256 = "eaa06c2e059ae0bd520acd67259a2ee29e15fc1751a566f4e2ac29ec345646f7"
PDFIUM_VERSION = "159.0.8146.0"


def stage_pdf_raster(wheel: Path, target: Path) -> None:
    if sys.platform != "win32" or digest(wheel) != PDFIUM_WHEEL_SHA256:
        raise RuntimeError("PDF raster requires the pinned pypdfium2 5.15.0 Windows amd64 wheel")
    target.mkdir()
    prefix = "pypdfium2-5.15.0.dist-info/licenses/"
    with zipfile.ZipFile(wheel) as archive:
        (target / "pdfium.dll").write_bytes(archive.read("pypdfium2_raw/pdfium.dll"))
        for name in archive.namelist():
            if name.startswith(prefix) and not name.endswith("/"):
                path = target / "licenses" / name[len(prefix):]
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(archive.read(name))
    shutil.copyfile(Path(__file__).resolve().parents[1] / "internal" / "pluginruntime" / "render_pdf_page.py", target / "render_pdf_page.py")


def stage(options: argparse.Namespace) -> dict:
    target = Path(options.target).expanduser().absolute()
    reject_links(target)
    target = target.resolve()
    python_packages = checked_source(options.python_packages_source, "Python packages source", True)
    fonts = checked_source(options.fonts_source, "fonts source", True)
    tesseract = checked_source(options.tesseract_source, "Tesseract source")
    wheel = Path(options.pdfium_wheel).resolve(strict=True) if options.pdfium_wheel else None
    if wheel and not tesseract:
        raise RuntimeError("PDF raster requires the reviewed Tesseract runtime")
    validate_python(python_packages)
    validate_fonts(fonts)
    if tesseract:
        validate_tesseract(tesseract)
    if target.exists():
        existing = verify_runtime(target)
        if wheel and existing.get("components", {}).get("pdf_raster") != PDFIUM_VERSION:
            raise RuntimeError("existing runtime has no pinned PDF raster support; use a fresh output directory")
        return existing
    staging = target.parent / f"{target.name}.staging-{uuid.uuid4().hex}"
    try:
        staging.mkdir(parents=True, exist_ok=False)
        copy_tree(python_packages, staging / "python-packages", python_runtime=True)
        patched_utils = patch_reportlab_offline(staging / "python-packages")
        copy_tree(fonts, staging / "fonts")
        if tesseract:
            copy_tree(tesseract, staging / "tesseract")
        if wheel:
            stage_pdf_raster(wheel, staging / "pdf-raster")
        files = {str(item.relative_to(staging)).replace("\\", "/"): digest(item) for item in staging.rglob("*") if item.is_file()}
        record = {
            "schema_version": 2,
            "platform": "windows-x64" if sys.platform == "win32" else sys.platform,
            "policy": "reportlab-offline-pypdf-reviewed-sources-v1",
            "components": {"python_packages": True, "reportlab": "4.4.9-offline-patch-v1", "reportlab_utils_sha256": patched_utils, "pypdf": "6.21.4", "pillow": False, "pymupdf": False, "embeddable_ttf": True, "tesseract_chi_sim": bool(tesseract), "libreoffice": False, "pdf_tools": False, "pdf_raster": PDFIUM_VERSION if wheel else False},
            "files": files,
        }
        (staging / "runtime-manifest.json").write_text(json.dumps(record, indent=2, sort_keys=True), encoding="utf-8")
        target.parent.mkdir(parents=True, exist_ok=True)
        staging.rename(target)
        return record
    finally:
        if staging.exists():
            shutil.rmtree(staging)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", required=True)
    parser.add_argument("--python-packages-source", required=True)
    parser.add_argument("--fonts-source", required=True)
    parser.add_argument("--tesseract-source")
    parser.add_argument("--pdfium-wheel")
    args = parser.parse_args()
    record = stage(args)
    print(json.dumps({"ready": True, "target": str(Path(args.target).resolve()), "components": record["components"]}, ensure_ascii=False))
