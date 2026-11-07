"""Offline contract tests. No models, networks, WeChat or real user files."""
import json
import base64
import binascii
from pathlib import Path
import sys
import zlib

sys.dont_write_bytecode = True  # Keep the signed package free of generated modules.
import tempfile
import unittest
from unittest.mock import patch
import zipfile

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "runtime"))
import document_logic as logic
import main
from reportlab_image import EmbeddedImageError, decode_png


class DocumentWorkshopTests(unittest.TestCase):
    def fixture(self):
        return json.loads((ROOT / "tests" / "fixtures" / "layout.json").read_text(encoding="utf8"))

    def test_pure_png_decoder_reads_rgb_without_pillow(self):
        def chunk(kind, value):
            return len(value).to_bytes(4, "big") + kind + value + binascii.crc32(kind + value).to_bytes(4, "big")

        # 2×1 RGB PNG, filter=0, written with stdlib only.
        png = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", (2).to_bytes(4, "big") + (1).to_bytes(4, "big") + b"\x08\x02\x00\x00\x00")
               + chunk(b"IDAT", zlib.compress(b"\x00\xff\x00\x00\x00\xff\x00")) + chunk(b"IEND", b""))
        image = decode_png(png)
        self.assertEqual((image["width"], image["height"], image["mode"]), (2, 1, "RGB"))
        self.assertEqual(image["data"], b"\xff\x00\x00\x00\xff\x00")
        with self.assertRaises(EmbeddedImageError):
            decode_png(b"not an image")

    def test_reconstruct_is_editable_docx_and_preserves_text_and_table(self):
        with tempfile.TemporaryDirectory() as raw:
            product = logic.reconstruct(self.fixture(), Path(raw), "notice.docx", "文档验收")
            path = Path(product["path"])
            self.assertTrue(zipfile.is_zipfile(path))
            self.assertIn("word/document.xml", zipfile.ZipFile(path).namelist())
            text = logic.text_content(path)
            self.assertIn("合成课程通知", text)
            self.assertIn("2026-11-03", text)
            verified = logic.basic_verify(path, ["课程通知", "文档转换验收"])
            self.assertTrue(verified["ok"])
            self.assertTrue(verified["editable"])

    def test_merged_table_round_trips_without_losing_cell_content(self):
        table = {
            "kind": "table",
            "rows": [["课程\n计划", "", ""], ["分组", "一", "二"], ["", "三", "四"]],
            "merges": [
                {"row": 0, "column": 0, "row_span": 1, "col_span": 3},
                {"row": 1, "column": 0, "row_span": 2, "col_span": 1},
            ],
        }
        with tempfile.TemporaryDirectory() as raw:
            path = Path(logic.reconstruct({"blocks": [table]}, Path(raw), "table.docx")["path"])
            self.assertEqual(logic._docx_flow_layout(path)["blocks"], [table])
            table["rows"][2][0] = "不能丢失的内容"
            with self.assertRaisesRegex(logic.DocumentError, "document_table_merge_invalid"):
                logic.reconstruct({"blocks": [table]}, Path(raw), "invalid.docx")

    def test_borderless_table_remains_editable_and_retains_rendering_style(self):
        from native_pdf import _blocks
        table = {"kind": "table", "rows": [["项目", "结果"], ["文件", "完成"]], "borderless": True}
        with tempfile.TemporaryDirectory() as raw:
            path = Path(logic.reconstruct({"blocks": [table]}, Path(raw), "table.docx")["path"])
            self.assertEqual(logic._docx_flow_layout(path)["blocks"], [table])
            self.assertEqual(_blocks(path), [("table", table["rows"], [], True)])
            with zipfile.ZipFile(path) as zf:
                self.assertEqual(zf.read("word/document.xml").count(b'w:val="nil"'), 6)

    def test_revise_title_preserves_body_tables_and_original(self):
        layout = {"blocks": [
            {"kind": "heading", "level": 1, "text": "旧文档标题"},
            {"kind": "paragraph", "text": "合成正文", "font_size_pt": 11},
            {"kind": "table", "rows": [["项目", "结果"], ["修改", "通过"]]},
        ]}
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            original = Path(logic.reconstruct(layout, root, "original.docx")["path"])
            digest = logic.file_digest(original)
            revised = Path(logic.revise_title(original, root, "revision.docx", 26.5)["path"])
            self.assertEqual(logic.file_digest(original), digest)
            self.assertNotEqual(logic.file_digest(revised), digest)
            self.assertEqual(logic.text_content(original), logic.text_content(revised))
            member = "word/document.xml"
            with zipfile.ZipFile(original) as before, zipfile.ZipFile(revised) as after:
                self.assertEqual(before.namelist(), after.namelist())
                for name in before.namelist():
                    if name != member:
                        self.assertEqual(before.read(name), after.read(name))
                new_xml = after.read(member)
                self.assertIn(b'<w:sz w:val="53"/>', new_xml)
                self.assertIn(b'<w:sz w:val="22"/>', new_xml)
                self.assertIn(b"<w:tbl>", new_xml)
            with self.assertRaisesRegex(logic.DocumentError, "document_font_size_invalid"):
                logic.revise_title(original, root, "invalid.docx", 22.2)
            no_title = Path(logic.reconstruct({"blocks": [{"kind": "paragraph", "text": "正文"}]}, root, "no-title.docx")["path"])
            with self.assertRaisesRegex(logic.DocumentError, "document_title_missing"):
                logic.revise_title(no_title, root, "not-created.docx", 24)
            self.assertFalse((root / "not-created.docx").exists())

    def test_docx_inspection_returns_editable_flow_layout_when_requested(self):
        layout = {
            "blocks": [
                {"kind": "title", "text": "课程通知"},
                {"kind": "heading", "level": 2, "text": "安排"},
                {"kind": "paragraph", "text": "请按时参加。"},
                {"kind": "table", "rows": [["日期", "内容"], ["周一", "验收"]]},
            ]
        }
        with tempfile.TemporaryDirectory() as raw:
            product = logic.reconstruct(layout, Path(raw), "flow.docx")
            view = logic.inspect(Path(product["path"]), include_layout=True)
            self.assertEqual(view["text"], "课程通知\n安排\n请按时参加。\n日期\n内容\n周一\n验收")
            self.assertEqual(view["layout"]["blocks"], layout["blocks"])
            coverage = view["layout"]["coverage"]
            self.assertEqual(coverage["kind"], "docx_flow")
            self.assertFalse(coverage["page_scoped"])
            self.assertTrue(coverage["eof"])

    def test_docx_page_selection_is_rejected_instead_of_ignored(self):
        with tempfile.TemporaryDirectory() as raw:
            product = logic.reconstruct(self.fixture(), Path(raw), "flow.docx")
            with self.assertRaises(logic.DocumentError) as error:
                logic.inspect(Path(product["path"]), pages=[1])
            self.assertEqual(str(error.exception), "document_docx_page_selection_unsupported")

    def test_docx_inspection_paginates_text_and_editable_flow_without_silent_cut(self):
        layout = {"blocks": [{"kind": "paragraph", "text": "第一段"}, {"kind": "paragraph", "text": "第二段"}, {"kind": "paragraph", "text": "第三段"}]}
        with tempfile.TemporaryDirectory() as raw:
            product = logic.reconstruct(layout, Path(raw), "paged.docx")
            path = Path(product["path"])
            first = logic.inspect(path, include_layout=True, text_limit=4, block_limit=2)
            self.assertEqual(first["text"], "第一段\n")
            self.assertEqual(first["next_text_offset"], 4)
            self.assertFalse(first["text_eof"])
            self.assertEqual([block["text"] for block in first["layout"]["blocks"]], ["第一段", "第二段"])
            self.assertEqual(first["layout"]["coverage"]["next_start_block"], 2)
            self.assertFalse(first["layout"]["coverage"]["eof"])
            second = logic.inspect(path, text_offset=first["next_text_offset"], text_limit=100, include_layout=True, start_block=2, block_limit=2)
            self.assertEqual(second["text"], "第二段\n第三段")
            self.assertTrue(second["text_eof"])
            self.assertEqual([block["text"] for block in second["layout"]["blocks"]], ["第三段"])
            self.assertTrue(second["layout"]["coverage"]["eof"])

    def test_docx_layout_reports_an_oversized_block_and_continuation(self):
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "large-block.docx"
            document = ('<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="%s"><w:body>'
                        '<w:p><w:r><w:t>%s</w:t></w:r></w:p>'
                        '<w:p><w:r><w:t>可读取</w:t></w:r></w:p><w:sectPr/></w:body></w:document>') % (logic.WORD_NS, "a" * 20_001)
            with zipfile.ZipFile(path, "w") as archive:
                archive.writestr("word/document.xml", document)
            first = logic.inspect(path, include_layout=True)
            coverage = first["layout"]["coverage"]
            self.assertEqual(first["layout"]["blocks"], [])
            self.assertEqual(coverage["unavailable_blocks"], [{"index": 0, "reason": "block_text_limit"}])
            self.assertEqual(coverage["next_start_block"], 1)
            second = logic.inspect(path, include_layout=True, start_block=1)
            self.assertEqual(second["layout"]["blocks"], [{"kind": "paragraph", "text": "可读取"}])
            self.assertTrue(second["layout"]["coverage"]["eof"])

    def test_unknown_or_scan_pdf_is_not_claimed_as_readable(self):
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "scan.pdf"
            path.write_bytes(b"%PDF-1.4\n1 0 obj<</Type /Page>>endobj\n%%EOF\n")
            view = logic.inspect(path, "application/pdf")
            self.assertTrue(view["scanned_or_no_extractable_text"])
            self.assertTrue(view["needs_clarification"])
            self.assertIn("document_runtime_inspection_required", view["quality_failures"])

    def test_docx_macro_and_external_relationships_are_rejected_before_text_read(self):
        with tempfile.TemporaryDirectory() as raw:
            for name, member, content, code in (
                ("macro.docx", "word/vbaProject.bin", b"not-code", "document_macro_not_allowed"),
                ("external.docx", "word/_rels/document.xml.rels", b'<Relationships><Relationship TargetMode="External" Target="https://example.invalid"/></Relationships>', "document_external_link_not_allowed"),
            ):
                path = Path(raw) / name
                with zipfile.ZipFile(path, "w") as archive:
                    archive.writestr("word/document.xml", '<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body/></w:document>')
                    archive.writestr(member, content)
                with self.assertRaises(logic.DocumentError) as error:
                    logic.text_content(path)
                self.assertEqual(str(error.exception), code)

    def test_mcp_refuses_raw_path_and_only_accepts_host_bound_artifact(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            product = logic.reconstruct(self.fixture(), root, "allowed.docx")
            meta = {"wechat/context": {"account": "a", "thread": "t", "requester": "u", "purpose": "document", "request_key": "k"},
                    "wechat/document": {"input_root": str(root), "output_root": str(root / "output"), "output_dir": str(root / "output"),
                                         "inputs": [{"artifact_id": "asset-safe", "path": product["path"], "mime_type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}]}}
            with self.assertRaises(logic.DocumentError) as blocked:
                main.call("document.verify", {"artifact": {"path": product["path"]}}, meta)
            self.assertEqual(str(blocked.exception), "document_artifact_required")
            good = main.call("document.verify", {"artifact_id": "asset-safe", "expected_text": ["合成课程通知"]}, meta)
            self.assertTrue(good["ok"])
            made = main.call("document.reconstruct", {"layout": self.fixture(), "filename": "new.docx"}, meta)
            self.assertTrue(made["ok"])
            self.assertEqual(made["outputs"][0]["role"], "editable_source")

    def test_layout_rejects_unknown_blocks(self):
        with tempfile.TemporaryDirectory() as raw:
            with self.assertRaises(logic.DocumentError) as error:
                logic.reconstruct({"blocks": [{"kind": "shell", "text": "ignored"}]}, Path(raw))
            self.assertEqual(str(error.exception), "document_layout_invalid")

    def test_export_reports_converter_unavailable_instead_of_fabricating_pdf(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            product = logic.reconstruct(self.fixture(), root, "source.docx")
            meta = {"wechat/context": {"account": "a", "thread": "t", "requester": "u", "purpose": "document", "request_key": "k"},
                    "wechat/document": {"input_root": str(root), "output_root": str(root / "output"), "output_dir": str(root / "output"),
                                         "inputs": [{"artifact_id": "asset-docx", "path": product["path"], "mime_type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}]}}
            # The normal runtime may be provisioned on an integration machine.
            # Isolate this contract: an unavailable converter must surface its
            # actual failure and never manufacture a PDF-shaped result.
            with patch.object(main, "_runtime", return_value={"ok": False, "code": "document_converter_unavailable"}):
                with self.assertRaises(logic.DocumentError) as error:
                    main.call("document.export_pdf", {"artifact": {"artifact_id": "asset-docx"}}, meta)
            self.assertEqual(str(error.exception), "document_converter_unavailable")

    def test_reconstruct_embeds_authorized_image_as_docx_media(self):
        png = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL1cQAAAABJRU5ErkJggg==")
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            image = root / "picture.png"
            image.write_bytes(png)
            layout = {"blocks": [{"kind": "paragraph", "text": "插图如下"}, {"kind": "image", "image_index": 0, "caption": "合成插图"}]}
            product = logic.reconstruct(layout, root, images=[image])
            with zipfile.ZipFile(product["path"]) as archive:
                self.assertIn("word/media/image1.png", archive.namelist())
                self.assertIn("word/_rels/document.xml.rels", archive.namelist())


if __name__ == "__main__":
    unittest.main()
