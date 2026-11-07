"""Synthetic regression checks for document runtime reuse and integrity."""
import argparse
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import prepare_document_runtime as runtime


class DocumentRuntimePreparationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.packages = self.root / "packages"
        self.utils = self.packages / "reportlab/lib/utils.py"
        self.utils.parent.mkdir(parents=True)
        self.utils.write_text("# Synthetic reviewed offline utility\n", encoding="utf-8")
        self.checksum = hashlib.sha256(self.utils.read_bytes()).hexdigest()
        pin = patch.object(runtime, "REPORTLAB_OFFLINE_UTILS_SHA256", self.checksum)
        pin.start()
        self.addCleanup(pin.stop)
        (self.packages / "reportlab/__init__.py").write_text('Version = "4.4.9"\n', encoding="utf-8")
        (self.packages / "pypdf").mkdir()
        (self.packages / "pypdf-6.21.4.dist-info").mkdir()
        self.fonts = self.root / "fonts"
        self.fonts.mkdir()
        (self.fonts / "synthetic.ttf").write_bytes(b"synthetic font fixture")
        (self.fonts / "OFL.txt").write_text("synthetic license fixture", encoding="utf-8")
        self.target = self.root / "output"
        self.options = argparse.Namespace(target=str(self.target), python_packages_source=str(self.packages),
            fonts_source=str(self.fonts), tesseract_source=None, pdfium_wheel=None)

    def test_reviewed_patch_is_reusable_without_rewriting(self):
        before = self.utils.read_bytes()
        self.assertEqual(runtime.patch_reportlab_offline(self.packages), self.checksum)
        self.assertEqual(runtime.patch_reportlab_offline(self.packages), self.checksum)
        self.assertEqual(self.utils.read_bytes(), before)
        self.utils.write_bytes(before + b"# changed\n")
        with self.assertRaises(RuntimeError):
            runtime.patch_reportlab_offline(self.packages)

    def test_stage_reuses_only_integrity_checked_existing_runtime(self):
        expected = runtime.stage(self.options)
        self.assertEqual(runtime.stage(self.options), expected)
        changed = self.target / "fonts/synthetic.ttf"
        changed.write_bytes(b"corrupted font")
        with self.assertRaisesRegex(RuntimeError, "integrity"):
            runtime.stage(self.options)
        self.assertEqual(changed.read_bytes(), b"corrupted font")

    def test_manifest_cannot_select_parent_or_alias_paths(self):
        for name in ["../outside.txt", "/outside.txt", "C:/outside.txt", "nested/../outside.txt", "nested//outside.txt", "nested\\outside.txt", ".. /outside.txt", "nested./outside.txt"]:
            with self.subTest(name=name):
                if not self.target.exists():
                    runtime.stage(self.options)
                manifest = self.target / "runtime-manifest.json"
                record = json.loads(manifest.read_text(encoding="utf-8"))
                record["files"] = {"python-packages/reportlab/lib/utils.py": self.checksum, name: "0" * 64}
                manifest.write_text(json.dumps(record), encoding="utf-8")
                with self.assertRaisesRegex(RuntimeError, "relative path"):
                    runtime.verify_runtime(self.target)

    def test_failed_patch_does_not_publish_partial_runtime(self):
        self.utils.write_text("unreviewed replacement", encoding="utf-8")
        with self.assertRaises(RuntimeError):
            runtime.stage(self.options)
        self.assertFalse(self.target.exists())
        self.assertEqual(list(self.root.glob("output.staging-*")), [])


if __name__ == "__main__":
    unittest.main()
