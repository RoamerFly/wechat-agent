import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock


SCRIPT = Path(__file__).with_name("stage_plugin_catalog.py")
spec = importlib.util.spec_from_file_location("stage_plugin_catalog", SCRIPT)
stage_plugin_catalog = importlib.util.module_from_spec(spec)
spec.loader.exec_module(stage_plugin_catalog)


def write_package(root, name="synthetic", extra=None):
    package = root / name
    runtime = package / "runtime"
    runtime.mkdir(parents=True)
    member = runtime / "main.py"
    member.write_bytes(b"print('synthetic')\n")
    files = {"runtime/main.py": hashlib.sha256(member.read_bytes()).hexdigest()}
    if extra:
        for relative, content in extra.items():
            path = package / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content)
            files[relative] = hashlib.sha256(content).hexdigest()
    (package / "plugin.json").write_text(json.dumps({"id": name, "files": files}), encoding="utf8")
    return package


def reindex(package):
    files = {}
    for path in sorted(package.rglob("*")):
        if path.is_file() and path.name != "plugin.json":
            files[path.relative_to(package).as_posix()] = hashlib.sha256(path.read_bytes()).hexdigest()
    (package / "plugin.json").write_text(json.dumps({"id": package.name, "files": files}), encoding="utf8")


class StagePluginCatalogTests(unittest.TestCase):
    @unittest.skipUnless(os.name == "nt", "Windows desktop build")
    def test_windows_desktop_rejects_retired_phone_files_without_overwriting(self):
        script = SCRIPT.with_name("Build-Desktop.ps1")
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            for member in ("plugins/mobile-control/old.marker", "runtimes/windows-music/mobile_bridge.py"):
                with self.subTest(member=member):
                    output = root / Path(member).name
                    marker = output / member
                    marker.parent.mkdir(parents=True)
                    marker.write_bytes(b"synthetic previous output")
                    result = subprocess.run(
                        ["powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
                         "-File", str(script), "-LocalStation", "-OutputDirectory", str(output)],
                        capture_output=True, creationflags=subprocess.CREATE_NO_WINDOW, timeout=30,
                    )
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(b"Output contains retired mobile components", result.stderr)
                    self.assertEqual(marker.read_bytes(), b"synthetic previous output")
                    self.assertFalse((output / "AgentHomeStation.exe").exists())

    def test_selected_package_stages_only_verified_members(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "plugins"
            selected = write_package(source, "share-reader")
            write_package(source, "media-understanding")
            write_package(source, "unrelated-tool")
            output = root / "bundle"
            result = stage_plugin_catalog.stage(source, output, selected="share-reader")
            self.assertEqual(result["packages"], ["share-reader"])
            self.assertEqual(sorted(path.name for path in output.iterdir()), ["share-reader"])
            self.assertTrue(stage_plugin_catalog.stage(source, output, selected="share-reader")["idempotent"])
            result = stage_plugin_catalog.stage(source, root / "selected-tools", selected=["share-reader", "media-understanding"])
            self.assertEqual(result["packages"], ["media-understanding", "share-reader"])
            with self.assertRaisesRegex(stage_plugin_catalog.StageError, "requested_package_unavailable"):
                stage_plugin_catalog.stage(source, root / "missing-tool", selected=["share-reader", "missing-tool"])
            (selected / "runtime" / "main.py").write_bytes(b"modified")
            with self.assertRaisesRegex(stage_plugin_catalog.StageError, "plugin_manifest_hash_mismatch"):
                stage_plugin_catalog.stage(source, root / "tampered", selected="share-reader")
            self.assertFalse((root / "tampered").exists())

    def test_rejects_source_output_overlap_before_source_changes(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "plugins"
            source.mkdir()
            package = write_package(source)
            before = {
                path.relative_to(source).as_posix(): path.read_bytes()
                for path in source.rglob("*") if path.is_file()
            }
            for output in (source, source / "catalog-output", root):
                with self.subTest(output=output), self.assertRaisesRegex(stage_plugin_catalog.StageError, "source_output_overlap"):
                    stage_plugin_catalog.stage(source, output)
                after = {
                    path.relative_to(source).as_posix(): path.read_bytes()
                    for path in source.rglob("*") if path.is_file()
                }
                self.assertEqual(after, before)
            self.assertFalse((source / "catalog-output").exists())
            self.assertTrue(package.exists())

    def test_stages_only_indexed_source_and_is_idempotent(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, output = root / "plugins", root / "portable" / "plugins"
            source.mkdir()
            write_package(source)
            first = stage_plugin_catalog.stage(source, output)
            self.assertFalse(first["idempotent"])
            self.assertEqual((output / "synthetic" / "runtime" / "main.py").read_bytes(), b"print('synthetic')\n")
            second = stage_plugin_catalog.stage(source, output)
            self.assertTrue(second["idempotent"])

    def test_rejects_hash_and_unindexed_source_before_output_write(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, output = root / "plugins", root / "out"
            source.mkdir()
            package = write_package(source)
            (package / "unexpected.txt").write_text("not indexed", encoding="utf8")
            with self.assertRaisesRegex(stage_plugin_catalog.StageError, "plugin_contains_unindexed_files"):
                stage_plugin_catalog.stage(source, output)
            self.assertFalse(output.exists())
            (package / "unexpected.txt").unlink()
            (package / "runtime" / "main.py").write_text("changed", encoding="utf8")
            with self.assertRaisesRegex(stage_plugin_catalog.StageError, "plugin_manifest_hash_mismatch"):
                stage_plugin_catalog.stage(source, output)
            self.assertFalse(output.exists())

    def test_existing_mismatch_requires_fresh_directory_without_deletion(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, output = root / "plugins", root / "out"
            source.mkdir()
            write_package(source)
            output.mkdir()
            marker = output / "keep.txt"
            marker.write_text("do not delete", encoding="utf8")
            with self.assertRaisesRegex(stage_plugin_catalog.StageError, "output_requires_fresh_directory"):
                stage_plugin_catalog.stage(source, output)
            self.assertEqual(marker.read_text(encoding="utf8"), "do not delete")

    def test_update_existing_replaces_only_indexed_members_and_is_idempotent(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, output = root / "source", root / "output"
            source.mkdir()
            package = write_package(source, extra={"runtime/obsolete.py": b"obsolete\n"})
            stage_plugin_catalog.stage(source, output)
            (package / "runtime" / "main.py").write_bytes(b"print('new')\n")
            (package / "runtime" / "obsolete.py").unlink()
            reindex(package)
            unrelated = output / "data"
            unrelated.mkdir()
            (unrelated / "user.txt").write_text("preserve", encoding="utf8")
            with self.assertRaisesRegex(stage_plugin_catalog.StageError, "output_catalog_unknown_intermediate_state"):
                stage_plugin_catalog.stage(source, output, update_existing=True)
            self.assertEqual((unrelated / "user.txt").read_text(encoding="utf8"), "preserve")
            (unrelated / "user.txt").unlink()
            unrelated.rmdir()
            result = stage_plugin_catalog.stage(source, output, update_existing=True)
            self.assertTrue(result["updated"])
            self.assertEqual((output / "synthetic" / "runtime" / "main.py").read_bytes(), b"print('new')\n")
            self.assertFalse((output / "synthetic" / "runtime" / "obsolete.py").exists())
            repeated = stage_plugin_catalog.stage(source, output, update_existing=True)
            self.assertTrue(repeated["idempotent"])

    def test_update_existing_rejects_tampered_old_catalog_before_write(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, output = root / "source", root / "output"
            source.mkdir()
            package = write_package(source)
            stage_plugin_catalog.stage(source, output)
            (output / "synthetic" / "runtime" / "main.py").write_bytes(b"tampered")
            (package / "runtime" / "main.py").write_bytes(b"new source")
            reindex(package)
            before = (output / "synthetic" / "runtime" / "main.py").read_bytes()
            with self.assertRaisesRegex(stage_plugin_catalog.StageError, "output_catalog_unknown_intermediate_state"):
                stage_plugin_catalog.stage(source, output, update_existing=True)
            self.assertEqual((output / "synthetic" / "runtime" / "main.py").read_bytes(), before)

    def test_update_existing_manifest_write_failure_can_retry_in_place(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, output = root / "source", root / "output"
            source.mkdir()
            package = write_package(source)
            stage_plugin_catalog.stage(source, output)
            old_manifest = (output / "synthetic" / "plugin.json").read_bytes()
            (package / "runtime" / "main.py").write_bytes(b"print('replacement')\n")
            reindex(package)
            real_replace = stage_plugin_catalog.os.replace

            def fail_manifest(source_path, destination):
                if Path(destination).name == "plugin.json":
                    raise OSError("synthetic disk failure")
                return real_replace(source_path, destination)

            with mock.patch.object(stage_plugin_catalog.os, "replace", side_effect=fail_manifest):
                with self.assertRaises(OSError):
                    stage_plugin_catalog.stage(source, output, update_existing=True)
            self.assertEqual((output / "synthetic" / "plugin.json").read_bytes(), old_manifest)
            result = stage_plugin_catalog.stage(source, output, update_existing=True)
            self.assertFalse(result["idempotent"])
            self.assertEqual((output / "synthetic" / "runtime" / "main.py").read_bytes(), b"print('replacement')\n")
            self.assertTrue(stage_plugin_catalog.stage(source, output, update_existing=True)["idempotent"])

    def test_rejects_manifest_path_traversal(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "plugins"
            source.mkdir()
            package = write_package(source)
            manifest = json.loads((package / "plugin.json").read_text(encoding="utf8"))
            manifest["files"] = {"../outside.py": "0" * 64}
            (package / "plugin.json").write_text(json.dumps(manifest), encoding="utf8")
            with self.assertRaisesRegex(stage_plugin_catalog.StageError, "invalid_plugin_manifest"):
                stage_plugin_catalog.stage(source, root / "out")

    def test_rejects_symlinked_catalog_root_when_supported(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "plugins"
            source.mkdir()
            write_package(source)
            link = root / "plugins-link"
            try:
                os.symlink(source, link, target_is_directory=True)
            except OSError:
                self.skipTest("symlink creation unavailable")
            with self.assertRaisesRegex(stage_plugin_catalog.StageError, "source_catalog_unavailable"):
                stage_plugin_catalog.stage(link, root / "out")

    def test_rejects_windows_junction_like_nested_member_directory(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "plugins"
            source.mkdir()
            write_package(source)
            real_is_junction = Path.is_junction

            def synthetic_junction(path):
                return path.name == "runtime" or real_is_junction(path)

            with mock.patch.object(Path, "is_junction", synthetic_junction):
                with self.assertRaisesRegex(stage_plugin_catalog.StageError, "plugin_links_forbidden"):
                    stage_plugin_catalog.stage(source, root / "out")

    def test_application_descriptor_uses_same_integrity_and_update_contract(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, output = root / "apps", root / "out"
            source.mkdir()
            package = write_package(source, name="wechat")
            (package / "plugin.json").rename(package / "app.json")
            first = stage_plugin_catalog.stage(source, output, descriptor="app.json")
            self.assertEqual(first["packages"], ["wechat"])
            self.assertFalse((output / "wechat" / "plugin.json").exists())
            self.assertTrue(stage_plugin_catalog.stage(source, output, descriptor="app.json")["idempotent"])
            member = package / "runtime" / "main.py"
            member.write_bytes(b"print('updated')\n")
            with self.assertRaisesRegex(stage_plugin_catalog.StageError, "hash_mismatch"):
                stage_plugin_catalog.stage(source, output, True, descriptor="app.json")
            manifest = package / "app.json"
            data = json.loads(manifest.read_text(encoding="utf8"))
            data["files"]["runtime/main.py"] = hashlib.sha256(member.read_bytes()).hexdigest()
            manifest.write_text(json.dumps(data), encoding="utf8")
            updated = stage_plugin_catalog.stage(source, output, True, descriptor="app.json")
            self.assertTrue(updated["updated"])
            self.assertEqual((output / "wechat" / "runtime" / "main.py").read_bytes(), member.read_bytes())
            self.assertTrue(stage_plugin_catalog.stage(source, output, True, descriptor="app.json")["idempotent"])

    def test_real_official_catalog_stages_packages_without_host_go_files(self):
        source = SCRIPT.parents[1] / "plugins"
        with tempfile.TemporaryDirectory() as temp:
            output = Path(temp) / "plugins"
            result = stage_plugin_catalog.stage(source, output)
            self.assertEqual(set(result["packages"]), {
                "audio-workshop", "comfyui", "document-workshop", "external-tools", "media-understanding", "mobile-control", "music", "obs-preview",
                "project-development", "rvc", "share-reader", "speech", "station-speech", "visual-generation", "web-search", "yue",
            })
            self.assertFalse((output / "catalog.go").exists())
            self.assertFalse((output / "catalog_test.go").exists())
            for package in result["packages"]:
                source_manifest = json.loads((source / package / "plugin.json").read_text(encoding="utf8"))
                for relative, digest in source_manifest["files"].items():
                    self.assertEqual(hashlib.sha256((output / package / relative).read_bytes()).hexdigest(), digest)


if __name__ == "__main__":
    unittest.main()
