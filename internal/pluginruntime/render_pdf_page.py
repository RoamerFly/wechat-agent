"""Render one host-selected PDF page under LPAC using the pinned PDFium DLL."""
import ctypes as c
import math
from pathlib import Path
import struct
import sys
import zlib


def png_chunk(tag, data):
    return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", zlib.crc32(tag + data))


def render(source, output, number, dpi):
    if number < 1 or not 96 <= dpi <= 300 or not 0 < source.stat().st_size <= 128 << 20:
        return 2
    pdfium = c.WinDLL(str(Path(__file__).with_name("pdfium.dll")), winmode=0x1100)

    def bind(name, result, *arguments):
        function = getattr(pdfium, ("FPDF" if name.startswith("Bitmap_") else "FPDF_") + name)
        function.argtypes = arguments
        function.restype = result
        return function

    ptr, integer = c.c_void_p, c.c_int
    load = bind("LoadMemDocument64", ptr, c.c_char_p, c.c_size_t, c.c_char_p)
    close = bind("CloseDocument", None, ptr)
    count = bind("GetPageCount", integer, ptr)
    get_page = bind("LoadPage", ptr, ptr, integer)
    close_page = bind("ClosePage", None, ptr)
    width_pt = bind("GetPageWidthF", c.c_float, ptr)
    height_pt = bind("GetPageHeightF", c.c_float, ptr)
    create = bind("Bitmap_CreateEx", ptr, integer, integer, integer, ptr, integer)
    fill = bind("Bitmap_FillRect", integer, ptr, integer, integer, integer, integer, c.c_ulong)
    draw = bind("RenderPageBitmap", None, ptr, ptr, integer, integer, integer, integer, integer, integer)
    destroy = bind("Bitmap_Destroy", None, ptr)
    bind("InitLibrary", None)()
    try:
        payload = source.read_bytes()
        doc = load(payload, len(payload), None)
        if not doc:
            return 5
        try:
            if number > count(doc):
                return 3
            page = get_page(doc, number - 1)
            if not page:
                return 5
            try:
                w, h = (math.ceil(size * dpi / 72) for size in (width_pt(page), height_pt(page)))
                if not 0 < w <= 8000 or not 0 < h <= 8000 or w * h > 16_000_000:
                    return 4
                stride = (w + 3) & ~3
                pixels = c.create_string_buffer(stride * h)
                # PDFium bitmap format 1 is an 8-bit gray channel. Use a
                # white background and exclude annotations from the page image.
                bitmap = create(w, h, 1, pixels, stride)
                if not bitmap:
                    return 5
                try:
                    if not fill(bitmap, 0, 0, w, h, 0xFFFFFFFF):
                        return 5
                    draw(bitmap, page, 0, 0, w, h, 0, 0)
                finally:
                    destroy(bitmap)
                compressed = zlib.compress(b"".join(b"\0" + pixels[y * stride : y * stride + w] for y in range(h)), 3)
                with output.open("xb") as file:
                    file.write(b"\x89PNG\r\n\x1a\n")
                    file.write(png_chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 0, 0, 0, 0)))
                    file.write(png_chunk(b"IDAT", compressed))
                    file.write(png_chunk(b"IEND", b""))
            finally:
                close_page(page)
        finally:
            close(doc)
    finally:
        bind("DestroyLibrary", None)()
    return 0


if __name__ == "__main__":
    try:
        status = render(Path(sys.argv[1]), Path(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4]))
    except (OSError, OverflowError, ValueError, AttributeError, IndexError):
        status = 5
    raise SystemExit(status)
