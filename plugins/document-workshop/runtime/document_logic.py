"""Safe, dependency-free document primitives for the document-workshop plugin.

This module deliberately receives paths only after the MCP bridge has checked a
host-bound artifact context.  It never interprets a model supplied path, URL or
command.  The DOCX writer uses a small OOXML subset so the offline fixture does
not depend on a global Python environment.
"""
from __future__ import annotations

import hashlib
import html
import json
import mimetypes
from pathlib import Path
import re
import shutil
import struct
import zipfile
from xml.dom import minidom
from xml.etree import ElementTree as ET
from xml.parsers.expat import ExpatError

MAX_FILE_BYTES = 128 * 1024 * 1024
MAX_TEXT = 250_000
MAX_DOCX_UNCOMPRESSED_BYTES = 64 * 1024 * 1024
MAX_DOCX_MEMBERS = 2_000
MAX_LAYOUT_BLOCKS = 500
MAX_LAYOUT_TEXT = 120_000
MAX_LAYOUT_TABLE_ROWS = 200
MAX_LAYOUT_CELLS_PER_ROW = 64
MAX_LAYOUT_CELL_TEXT = 20_000
DEFAULT_TEXT_LIMIT = 4_000
MAX_TEXT_LIMIT = 100_000
DEFAULT_BLOCK_LIMIT = 20
MAX_BLOCK_LIMIT = 100
MAX_LAYOUT_RESPONSE_BYTES = 512 * 1024
WORD_NS = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
REL_NS = "http://schemas.openxmlformats.org/package/2006/relationships"


class DocumentError(ValueError):
    pass


def file_digest(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for part in iter(lambda: f.read(1024 * 1024), b""):
            h.update(part)
    return h.hexdigest()


def safe_filename(value: str, suffix: str) -> str:
    if not isinstance(value, str):
        value = "document"
    value = re.sub(r"[^0-9A-Za-z._\-\u3400-\u9fff]+", "_", value).strip("._")[:96] or "document"
    return value + suffix if not value.lower().endswith(suffix) else value


def text_content(path: Path) -> str:
    """Extract editable text only; raises a stable code for unknown formats."""
    suffix = path.suffix.lower()
    if suffix == ".docx":
        # The container itself has a verified uncompressed-size limit. Do not
        # silently cut editable source text here; MCP inspection paginates it.
        return _whole_docx_text(path)
    if suffix == ".pdf":
        raw = path.read_bytes()
        if not raw.startswith(b"%PDF-"):
            raise DocumentError("document_pdf_invalid")
        # PDF text must be extracted by the project-local Poppler runtime.
        # A byte-level fallback would misreport compressed/CJK text as facts.
        return ""
    raise DocumentError("document_text_extraction_unsupported")


def _docx_paragraph_text(node: ET.Element) -> str:
    """Return the editable text represented by one Word paragraph node."""
    w = "{%s}" % WORD_NS
    parts = []
    for part in node.iter():
        if part.tag == w + "t":
            parts.append(part.text or "")
        elif part.tag in (w + "br", w + "cr"):
            parts.append("\n")
        elif part.tag == w + "tab":
            parts.append("\t")
    return "".join(parts)


def _docx_style(node: ET.Element) -> str:
    properties = node.find("{%s}pPr" % WORD_NS)
    style = properties.find("{%s}pStyle" % WORD_NS) if properties is not None else None
    return (style.get("{%s}val" % WORD_NS) if style is not None else "") or ""


def _docx_block_for_paragraph(node: ET.Element) -> dict | None:
    text = _docx_paragraph_text(node)
    if not text:
        return None
    style = _docx_style(node)
    normalised = style.casefold().replace(" ", "")
    if normalised == "title":
        return {"kind": "title", "text": text}
    heading = re.fullmatch(r"heading([1-9])", normalised)
    if heading:
        return {"kind": "heading", "level": int(heading.group(1)), "text": text}
    return {"kind": "paragraph", "text": text}


def _docx_block_for_table(node: ET.Element) -> dict | None:
    rows, merges = [], []
    active = {}
    w = "{%s}" % WORD_NS
    for r, row in enumerate(node.findall(w + "tr")):
        cells, continued = [], {}
        for cell in row.findall(w + "tc"):
            value = "\n".join(
                value for value in (_docx_paragraph_text(paragraph) for paragraph in cell.findall(w + "p"))
                if value
            )
            width = cell.find(w + "tcPr/" + w + "gridSpan")
            vertical = cell.find(w + "tcPr/" + w + "vMerge")
            col = len(cells)
            try:
                span = int(width.get(w + "val", "1")) if width is not None else 1
            except ValueError:
                raise DocumentError("document_table_merge_invalid")
            if span < 1 or col + span > MAX_LAYOUT_CELLS_PER_ROW:
                raise DocumentError("document_table_merge_invalid")
            if vertical is not None and vertical.get(w + "val", "continue") == "continue":
                prior = active.get(col)
                if prior is None or prior["col_span"] != span or value:
                    raise DocumentError("document_table_merge_invalid")
                prior["row_span"] += 1
                continued[col] = prior
            elif vertical is not None or span > 1:
                merge = {"row": r, "column": col, "row_span": 1, "col_span": span}
                merges.append(merge)
                if vertical is not None:
                    if vertical.get(w + "val") != "restart":
                        raise DocumentError("document_table_merge_invalid")
                    continued[col] = merge
            cells.extend([value] + [""] * (span - 1))
        if cells:
            rows.append(cells)
        active = continued
    if not rows:
        return None
    result = {"kind": "table", "rows": rows}
    borders = node.find(w + "tblPr/" + w + "tblBorders")
    if borders is not None and len(borders) == 6 and all(side.get(w + "val") == "nil" for side in borders):
        result["borderless"] = True
    merges = [merge for merge in merges if merge["row_span"] > 1 or merge["col_span"] > 1]
    if merges:
        _table_coverage(rows, merges)
        result["merges"] = merges
    return result


def _docx_flow_layout(path: Path) -> dict:
    """Read actual Word flow blocks, with explicit bounds and coverage.

    OOXML DOCX has flowing content rather than a stable page model: pagination
    depends on fonts and the renderer.  This deliberately reports only editable
    paragraph/title/heading/table blocks, in body order, and never claims a
    page slice before conversion to PDF.
    """
    try:
        with zipfile.ZipFile(path) as zf:
            _verify_docx_container(zf)
            raw = zf.read("word/document.xml")
    except (OSError, KeyError, zipfile.BadZipFile):
        raise DocumentError("document_docx_invalid")
    try:
        root = ET.fromstring(raw)
    except ET.ParseError:
        raise DocumentError("document_docx_invalid")
    body = root.find("{%s}body" % WORD_NS)
    if body is None:
        raise DocumentError("document_docx_invalid")

    blocks = []
    for node in body:
        if node.tag == "{%s}p" % WORD_NS:
            block = _docx_block_for_paragraph(node)
        elif node.tag == "{%s}tbl" % WORD_NS:
            block = _docx_block_for_table(node)
        else:
            continue
        if block is None:
            continue
        blocks.append(block)
    return {"blocks": blocks}


def _whole_docx_text(path: Path) -> str:
    """Return the complete editable DOCX text for inspection pagination."""
    try:
        with zipfile.ZipFile(path) as zf:
            _verify_docx_container(zf)
            raw = zf.read("word/document.xml")
    except (OSError, KeyError, zipfile.BadZipFile):
        raise DocumentError("document_docx_invalid")
    try:
        root = ET.fromstring(raw)
    except ET.ParseError:
        raise DocumentError("document_docx_invalid")
    paragraphs = []
    for paragraph in root.findall(".//{%s}p" % WORD_NS):
        value = _docx_paragraph_text(paragraph)
        if value:
            paragraphs.append(value)
    return "\n".join(paragraphs)


def _nonnegative_int(value, default: int, maximum: int, code: str) -> int:
    if value is None:
        return default
    if type(value) is not int or value < 0 or value > maximum:
        raise DocumentError(code)
    return value


def _layout_block_issue(block: dict) -> str | None:
    if block["kind"] == "table":
        rows = block["rows"]
        if len(rows) > MAX_LAYOUT_TABLE_ROWS or any(len(row) > MAX_LAYOUT_CELLS_PER_ROW for row in rows):
            return "table_shape_limit"
        if any(len(cell) > MAX_LAYOUT_CELL_TEXT for row in rows for cell in row):
            return "table_cell_text_limit"
    elif len(block["text"]) > MAX_LAYOUT_CELL_TEXT:
        return "block_text_limit"
    return None


def _paginate_docx_layout(path: Path, start_block=None, block_limit=None) -> dict:
    flow = _docx_flow_layout(path)["blocks"]
    start = _nonnegative_int(start_block, 0, len(flow), "document_start_block_invalid")
    limit = _nonnegative_int(block_limit, DEFAULT_BLOCK_LIMIT, MAX_BLOCK_LIMIT, "document_block_limit_invalid")
    if limit == 0:
        raise DocumentError("document_block_limit_invalid")
    selected = []
    selected_bytes = 0
    unavailable = []
    index = start
    while index < len(flow) and len(selected) < limit:
        block = flow[index]
        issue = _layout_block_issue(block)
        block_bytes = len(json.dumps(block, ensure_ascii=False, separators=(",", ":")).encode("utf8"))
        if issue or block_bytes > MAX_LAYOUT_RESPONSE_BYTES:
            unavailable.append({"index": index, "reason": issue or "block_response_limit"})
            index += 1
            break
        if selected_bytes + block_bytes > MAX_LAYOUT_RESPONSE_BYTES:
            break
        selected.append(block)
        selected_bytes += block_bytes
        index += 1
    eof = index >= len(flow)
    coverage = {
        "kind": "docx_flow",
        "page_scoped": False,
        "start_block": start,
        "block_limit": limit,
        "returned_blocks": len(selected),
        "total_editable_blocks": len(flow),
        "returned_utf8_bytes": selected_bytes,
        "eof": eof,
        "limits": {
            "block_limit": MAX_BLOCK_LIMIT,
            "response_utf8_bytes": MAX_LAYOUT_RESPONSE_BYTES,
            "table_rows": MAX_LAYOUT_TABLE_ROWS,
            "table_cells_per_row": MAX_LAYOUT_CELLS_PER_ROW,
            "table_cell_characters": MAX_LAYOUT_CELL_TEXT,
        },
    }
    if not eof:
        coverage["next_start_block"] = index
    if unavailable:
        coverage["unavailable_blocks"] = unavailable
    return {"blocks": selected, "coverage": coverage}


def _verify_docx_container(zf: zipfile.ZipFile) -> None:
    entries = zf.infolist()
    if len(entries) > MAX_DOCX_MEMBERS or sum(entry.file_size for entry in entries) > MAX_DOCX_UNCOMPRESSED_BYTES:
        raise DocumentError("document_docx_expansion_limit")
    names = {entry.filename for entry in entries}
    if "word/vbaProject.bin" in names or any(name.lower().endswith("/vbaProject.bin".lower()) for name in names):
        raise DocumentError("document_macro_not_allowed")
    # Relationships with TargetMode=External would turn opening a document into
    # an external fetch. This plugin treats source docs as data, never as a
    # vehicle for external links.
    for name in names:
        if not name.endswith(".rels"):
            continue
        try:
            relationships = ET.fromstring(zf.read(name))
        except (KeyError, ET.ParseError):
            raise DocumentError("document_docx_invalid")
        if any(node.get("TargetMode") == "External" for node in relationships):
            raise DocumentError("document_external_link_not_allowed")


def png_size(path: Path):
    with path.open("rb") as source:
        raw = source.read(24)
    if raw[:8] != b"\x89PNG\r\n\x1a\n" or raw[12:16] != b"IHDR":
        return None
    return struct.unpack(">II", raw[16:24])


def inspect(path: Path, mime_type: str = "", pages=None, include_layout=False, text_offset=None, text_limit=None, start_block=None, block_limit=None) -> dict:
    suffix = path.suffix.lower()
    result = {
        "kind": {".docx": "docx", ".pdf": "pdf"}.get(suffix, "image" if mime_type.startswith("image/") else "unknown"),
        "sha256": file_digest(path), "bytes": path.stat().st_size,
        "evidence": [{"source": "artifact", "sha256": file_digest(path), "range": "whole_file"}],
        "untrusted_document_content": True,
    }
    if result["bytes"] > MAX_FILE_BYTES:
        raise DocumentError("document_too_large")
    if suffix == ".docx":
        # DOCX stores a continuous flow.  Stable pages are known only after a
        # particular renderer lays it out, so accepting this parameter here
        # would falsely imply that a page range was honored.
        if pages:
            raise DocumentError("document_docx_page_selection_unsupported")
        offset = _nonnegative_int(text_offset, 0, MAX_DOCX_UNCOMPRESSED_BYTES, "document_text_offset_invalid")
        limit = _nonnegative_int(text_limit, DEFAULT_TEXT_LIMIT, MAX_TEXT_LIMIT, "document_text_limit_invalid")
        if limit == 0:
            raise DocumentError("document_text_limit_invalid")
        whole_text = _whole_docx_text(path)
        if offset > len(whole_text):
            raise DocumentError("document_text_offset_invalid")
        text = whole_text[offset:offset + limit]
        result.update({
            "text": text,
            "text_characters": len(text),
            "text_total_characters": len(whole_text),
            "text_offset": offset,
            "text_limit": limit,
            "text_eof": offset + len(text) >= len(whole_text),
            "needs_clarification": False,
        })
        if not result["text_eof"]:
            result["next_text_offset"] = offset + len(text)
        with zipfile.ZipFile(path) as zf:
            _verify_docx_container(zf)
            names = zf.namelist()
        result["embedded_images"] = sum(name.startswith("word/media/") for name in names)
        result["editable"] = True
        if include_layout:
            result["layout"] = _paginate_docx_layout(path, start_block, block_limit)
        return result
    if suffix == ".pdf":
        raw = path.read_bytes()
        if not raw.startswith(b"%PDF-"):
            raise DocumentError("document_pdf_invalid")
        result.update({"text": "", "text_characters": 0, "page_count_estimate": len(re.findall(rb"/Type\s*/Page\b", raw)), "scanned_or_no_extractable_text": True, "needs_clarification": True, "quality_failures": ["document_runtime_inspection_required"]})
        return result
    if mime_type.startswith("image/") or suffix in (".png", ".jpg", ".jpeg", ".webp"):
        result["needs_clarification"] = True
        result["quality_failures"] = ["document_ocr_required"]
        size = png_size(path) if suffix == ".png" else None
        if size:
            result["pixel_width"], result["pixel_height"] = size
        if include_layout:
            result["layout_observation"] = "ocr_not_run_by_plugin_runtime"
        return result
    raise DocumentError("document_format_unsupported")


def _xml(value: str) -> str:
    return html.escape(value, quote=False)


def _paragraph(text: str, style: str = "", font_size_pt: float | None = None) -> str:
    ppr = "<w:pPr><w:pStyle w:val=\"%s\"/></w:pPr>" % style if style else ""
    chunks = text.split("\n") or [""]
    size = "<w:rPr><w:sz w:val=\"%d\"/></w:rPr>" % round(font_size_pt * 2) if font_size_pt is not None else ""
    runs = "".join("<w:r>%s<w:t xml:space=\"preserve\">%s</w:t></w:r>" % (size, _xml(chunk)) + ("<w:r><w:br/></w:r>" if i < len(chunks)-1 else "") for i, chunk in enumerate(chunks))
    return "<w:p>%s%s</w:p>" % (ppr, runs)


def _table_coverage(rows, merges) -> dict:
    """Map covered coordinates to their validated, rectangular merge."""
    if not isinstance(merges, list) or not rows or not isinstance(rows[0], list):
        raise DocumentError("document_table_merge_invalid")
    cols = len(rows[0])
    if not 1 <= cols <= MAX_LAYOUT_CELLS_PER_ROW or any(not isinstance(row, list) or len(row) != cols for row in rows):
        raise DocumentError("document_table_merge_invalid")
    coverage = {}
    keys = ("row", "column", "row_span", "col_span")
    for merge in merges:
        if not isinstance(merge, dict) or set(merge) != set(keys) or any(type(merge[key]) is not int for key in keys):
            raise DocumentError("document_table_merge_invalid")
        r, c, h, w = (merge[key] for key in keys)
        if min(r, c) < 0 or min(h, w) < 1 or h * w < 2 or r+h > len(rows) or c+w > cols:
            raise DocumentError("document_table_merge_invalid")
        for y in range(r, r+h):
            for x in range(c, c+w):
                if (y, x) in coverage or (y, x) != (r, c) and rows[y][x] != "":
                    raise DocumentError("document_table_merge_invalid")
                coverage[y, x] = (r, c, h, w)
    return coverage


def _table(rows, merges=None, borderless=False) -> str:
    if type(borderless) is not bool:
        raise DocumentError("document_table_borderless_invalid")
    style = 'w:val="nil"' if borderless else 'w:val="single" w:sz="4"'
    borders = "".join("<w:%s %s/>" % (side, style) for side in ("top", "left", "bottom", "right", "insideH", "insideV"))
    parts = ["<w:tbl><w:tblPr><w:tblBorders>%s</w:tblBorders></w:tblPr>" % borders]
    coverage = _table_coverage(rows, merges) if merges is not None else {}
    if coverage:
        parts.append("<w:tblGrid>%s</w:tblGrid>" % ("<w:gridCol/>" * len(rows[0])))
    for r, row in enumerate(rows):
        if not isinstance(row, list):
            continue
        cells = []
        for c, text in enumerate(row):
            if (r, c) not in coverage:
                cells.append("<w:tc><w:tcPr/>%s</w:tc>" % _paragraph(str(text)))
                continue
            origin_r, origin_c, height, width = coverage[r, c]
            if c != origin_c:
                continue
            props = '<w:gridSpan w:val="%d"/>' % width if width > 1 else ""
            if height > 1:
                props += '<w:vMerge w:val="restart"/>' if r == origin_r else "<w:vMerge/>"
            cells.append("<w:tc><w:tcPr>%s</w:tcPr>%s</w:tc>" % (props, _paragraph(str(text))))
        parts.append("<w:tr>%s</w:tr>" % "".join(cells))
    return "".join(parts) + "</w:tbl>"


def _picture(rel_id: str, index: int, name: str, width: int = 4572000, height: int = 2743200) -> str:
    """A native DrawingML inline picture. It remains a real DOCX image part."""
    return ("<w:p><w:r><w:drawing><wp:inline xmlns:wp=\"http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing\" "
            "xmlns:a=\"http://schemas.openxmlformats.org/drawingml/2006/main\" xmlns:pic=\"http://schemas.openxmlformats.org/drawingml/2006/picture\">"
            "<wp:extent cx=\"%d\" cy=\"%d\"/><wp:docPr id=\"%d\" name=\"%s\"/><a:graphic><a:graphicData uri=\"http://schemas.openxmlformats.org/drawingml/2006/picture\">"
            "<pic:pic><pic:nvPicPr><pic:cNvPr id=\"%d\" name=\"%s\"/><pic:cNvPicPr/></pic:nvPicPr><pic:blipFill><a:blip r:embed=\"%s\" xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill>"
            "<pic:spPr><a:xfrm><a:off x=\"0\" y=\"0\"/><a:ext cx=\"%d\" cy=\"%d\"/></a:xfrm><a:prstGeom prst=\"rect\"><a:avLst/></a:prstGeom></pic:spPr>"
            "</pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p>") % (width, height, index, _xml(name), index, _xml(name), rel_id, width, height)


def reconstruct(layout: dict, output_dir: Path, filename: str = "document.docx", title: str = "", images=None, title_font_size_pt: float | None = None) -> dict:
    if not isinstance(layout, dict):
        raise DocumentError("document_layout_invalid")
    blocks = layout.get("blocks", [])
    if not isinstance(blocks, list) or len(blocks) > 500:
        raise DocumentError("document_layout_invalid")
    body = []
    if title:
        if title_font_size_pt is not None and (type(title_font_size_pt) not in (int, float) or not 8 <= title_font_size_pt <= 48):
            raise DocumentError("document_font_size_invalid")
        body.append(_paragraph(title[:256], "Title", title_font_size_pt))
    for block in blocks:
        if not isinstance(block, dict) or set(block) - {"kind", "text", "rows", "level", "image_index", "caption", "font_size_pt", "merges", "borderless"}:
            raise DocumentError("document_layout_invalid")
        kind = block.get("kind")
        if kind in ("paragraph", "heading", "title"):
            text = block.get("text")
            if not isinstance(text, str) or len(text) > 20_000:
                raise DocumentError("document_layout_invalid")
            level = block.get("level", 1)
            if kind == "heading" and (type(level) is not int or not 1 <= level <= 9):
                raise DocumentError("document_layout_invalid")
            style = "Title" if kind == "title" else "Heading%d" % level if kind == "heading" else ""
            size = block.get("font_size_pt")
            if size is not None and (type(size) not in (int, float) or not 8 <= size <= 48):
                raise DocumentError("document_font_size_invalid")
            body.append(_paragraph(text, style, size))
        elif kind == "table":
            rows = block.get("rows")
            if not isinstance(rows, list) or len(rows) > 200:
                raise DocumentError("document_layout_invalid")
            body.append(_table(rows, block.get("merges"), block.get("borderless", False)))
        elif kind == "image":
            image_index = block.get("image_index")
            if type(image_index) is not int or not isinstance(images, list) or not 0 <= image_index < len(images):
                raise DocumentError("document_image_not_authorized")
            picture = images[image_index]
            size = png_size(picture) if picture.suffix.lower() == ".png" else None
            if size is not None and min(size) > 0:
                scale = min(4572000 / size[0], 5486400 / size[1])
                body.append(_picture("rId%d" % (image_index + 1), image_index + 1, picture.name,
                                     max(1, round(size[0] * scale)), max(1, round(size[1] * scale))))
            else:
                body.append(_picture("rId%d" % (image_index + 1), image_index + 1, picture.name))
            if isinstance(block.get("caption"), str) and block["caption"]:
                body.append(_paragraph(block["caption"][:500]))
        else:
            raise DocumentError("document_layout_invalid")
    if not body:
        raise DocumentError("document_layout_empty")
    output_dir.mkdir(parents=True, exist_ok=True)
    target = output_dir / safe_filename(filename, ".docx")
    document = """<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?><w:document xmlns:w=\"%s\"><w:body>%s<w:sectPr><w:pgSz w:w=\"11906\" w:h=\"16838\"/><w:pgMar w:top=\"1440\" w:right=\"1440\" w:bottom=\"1440\" w:left=\"1440\"/></w:sectPr></w:body></w:document>""" % (WORD_NS, "".join(body))
    types = """<?xml version=\"1.0\" encoding=\"UTF-8\"?><Types xmlns=\"http://schemas.openxmlformats.org/package/2006/content-types\"><Default Extension=\"rels\" ContentType=\"application/vnd.openxmlformats-package.relationships+xml\"/><Default Extension=\"xml\" ContentType=\"application/xml\"/><Default Extension=\"png\" ContentType=\"image/png\"/><Default Extension=\"jpg\" ContentType=\"image/jpeg\"/><Default Extension=\"jpeg\" ContentType=\"image/jpeg\"/><Override PartName=\"/word/document.xml\" ContentType=\"application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml\"/><Override PartName=\"/word/styles.xml\" ContentType=\"application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml\"/></Types>"""
    rels = """<?xml version=\"1.0\" encoding=\"UTF-8\"?><Relationships xmlns=\"%s\"><Relationship Id=\"rId1\" Type=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument\" Target=\"word/document.xml\"/></Relationships>""" % REL_NS
    styles = """<?xml version=\"1.0\" encoding=\"UTF-8\"?><w:styles xmlns:w=\"%s\"><w:style w:type=\"paragraph\" w:default=\"1\" w:styleId=\"Normal\"><w:name w:val=\"Normal\"/></w:style><w:style w:type=\"paragraph\" w:styleId=\"Title\"><w:name w:val=\"Title\"/><w:rPr><w:b/><w:sz w:val=\"36\"/></w:rPr></w:style>%s</w:styles>""" % (WORD_NS, "".join("<w:style w:type=\"paragraph\" w:styleId=\"Heading%d\"><w:name w:val=\"heading %d\"/><w:rPr><w:b/></w:rPr></w:style>" % (i, i) for i in range(1, 10)))
    doc_rels = "<?xml version=\"1.0\" encoding=\"UTF-8\"?><Relationships xmlns=\"%s\">%s</Relationships>" % (REL_NS, "".join("<Relationship Id=\"rId%d\" Type=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships/image\" Target=\"media/image%d%s\"/>" % (i+1, i+1, image.suffix.lower()) for i, image in enumerate(images or [])))
    with zipfile.ZipFile(target, "w", zipfile.ZIP_DEFLATED) as zf:
        zf.writestr("[Content_Types].xml", types)
        zf.writestr("_rels/.rels", rels)
        zf.writestr("word/document.xml", document)
        zf.writestr("word/styles.xml", styles)
        if images:
            zf.writestr("word/_rels/document.xml.rels", doc_rels)
            for i, image in enumerate(images):
                zf.write(image, "word/media/image%d%s" % (i+1, image.suffix.lower()))
    return {"path": str(target), "mime_type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "sha256": file_digest(target), "text": text_content(target)}


def basic_verify(path: Path, expected_text=None) -> dict:
    suffix = path.suffix.lower()
    result = {"sha256": file_digest(path), "bytes": path.stat().st_size, "quality_failures": []}
    if suffix == ".docx":
        text = text_content(path)
        result.update({"format": "docx", "editable": True, "text_characters": len(text)})
    elif suffix == ".pdf":
        raw = path.read_bytes()
        if not raw.startswith(b"%PDF-"):
            raise DocumentError("document_pdf_invalid")
        result.update({"format": "pdf", "page_count_estimate": len(re.findall(rb"/Type\s*/Page\b", raw)), "text_characters": 0})
        if not result["page_count_estimate"]:
            result["quality_failures"].append("document_pdf_page_count_unknown")
    else:
        raise DocumentError("document_format_unsupported")
    for token in expected_text or []:
        if isinstance(token, str) and token and (suffix == ".pdf" or token not in text):
            result["quality_failures"].append("document_expected_text_missing")
            break
    result["ok"] = not result["quality_failures"]
    return result


def revise_title(source: Path, output: Path, filename: str, font_size_pt: float) -> dict:
    """Change only the runs of the existing main title in a workshop DOCX."""
    if type(font_size_pt) not in (int, float) or not 8 <= font_size_pt <= 48 or font_size_pt * 2 % 1:
        raise DocumentError("document_font_size_invalid")
    target = output / safe_filename(filename, ".docx")
    member = "word/document.xml"
    try:
        with zipfile.ZipFile(source) as original:
            _verify_docx_container(original)
            if original.namelist().count(member) != 1:
                raise DocumentError("document_docx_invalid")
            xml = original.read(member)
            if b"<!DOCTYPE" in xml or b"<!ENTITY" in xml:
                raise DocumentError("document_docx_invalid")
            with minidom.parseString(xml) as doc:
                def children(node, kind):
                    return [child for child in node.childNodes if child.namespaceURI == WORD_NS and child.localName == kind]

                bodies = children(doc.documentElement, "body")
                if len(bodies) != 1:
                    raise DocumentError("document_docx_invalid")
                title = None
                for paragraph in children(bodies[0], "p"):
                    properties = children(paragraph, "pPr")
                    styles = children(properties[0], "pStyle") if properties else []
                    if styles and styles[0].getAttributeNS(WORD_NS, "val") in ("Title", "Heading1"):
                        title = paragraph
                        break
                if title is None:
                    raise DocumentError("document_title_missing")
                runs = [run for run in children(title, "r") if children(run, "t")]
                if not runs:
                    raise DocumentError("document_title_missing")
                for run in runs:
                    prefix = run.prefix or "w"
                    properties = children(run, "rPr")
                    props = properties[0] if properties else doc.createElementNS(WORD_NS, prefix + ":rPr")
                    if not properties:
                        run.insertBefore(props, run.firstChild)
                    sizes = children(props, "sz")
                    if not sizes:
                        size = doc.createElementNS(WORD_NS, prefix + ":sz")
                        following = next(iter(children(props, "szCs")), None)
                        props.insertBefore(size, following)
                        sizes.append(size)
                    for size in sizes + children(props, "szCs"):
                        size.setAttributeNS(WORD_NS, prefix + ":val", str(int(font_size_pt * 2)))
                changed = doc.toxml(encoding="utf-8")
            with zipfile.ZipFile(target, "x") as revised:
                for entry in original.infolist():
                    revised.writestr(entry, changed if entry.filename == member else original.read(entry))
    except (KeyError, OSError, ExpatError, zipfile.BadZipFile):
        raise DocumentError("document_title_revision_failed")
    return {"path": str(target), "sha256": file_digest(target), "font_size_pt": font_size_pt}
