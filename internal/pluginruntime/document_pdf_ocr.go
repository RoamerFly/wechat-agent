package pluginruntime

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

const (
	pdfRasterVersion = "159.0.8146.0"
	pdfiumSHA256     = "d52ce560196f14aeea3d558b6a7b2a668ce4cbfc156ec4bc2ccd6595066fbec9"
	maxRasterBytes   = 32 << 20
)

//go:embed render_pdf_page.py
var trustedPDFRasterScript []byte

type DocumentPDFOCRRequest struct {
	RuntimeRoot, InputPath, InputSHA256 string
	Page, DPI                           int
}

func RunDocumentPDFOCR(ctx context.Context, spec LaunchSpec, req DocumentPDFOCRRequest) (DocumentOCRObservation, error) {
	var empty DocumentOCRObservation
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if runtime.GOOS != "windows" || validateSpec(spec) != nil || req.Page < 1 || req.DPI < 96 || req.DPI > 300 {
		return empty, errors.New("pdf_scan_invalid_scope")
	}
	root, err := absoluteDir(req.RuntimeRoot)
	if err != nil || !inRoots(root, spec.RuntimeRoots) {
		return empty, errors.New("pdf_scan_unavailable")
	}
	script, err := verifiedPDFRaster(root)
	if err != nil {
		return empty, err
	}
	input, err := absoluteFile(req.InputPath)
	if err != nil || !containsExactPath(spec.ReadOnlyFiles, input) || !isSHA256(req.InputSHA256) || hashFile(input) != req.InputSHA256 {
		return empty, errors.New("pdf_scan_input_changed")
	}
	// The renderer and OCR run as separate initial LPAC processes. The latter
	// receives only the raster, and cannot change the PDF or any runtime file.
	output := filepath.Join(spec.DataDir, "scan-page.png")
	defer os.Remove(output)
	argv := []string{spec.Python, "-I", script, input, output, strconv.Itoa(req.Page), strconv.Itoa(req.DPI)}
	cmd, cleanup, err := isolatedCommand(spec, argv)
	if err != nil {
		return empty, errors.New("pdf_scan_render_unavailable")
	}
	defer cleanup()
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	renderCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	if err := cmd.Start(); err != nil {
		return empty, errors.New("pdf_scan_render_unavailable")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-renderCtx.Done():
		terminate(cmd)
		<-done
		return empty, errors.New("pdf_scan_render_timeout")
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 4 {
			return empty, errors.New("pdf_scan_raster_too_large")
		}
		return empty, errors.New("pdf_scan_render_failed")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	digest, err := verifiedPDFRasterImage(output)
	if err != nil {
		return empty, err
	}
	if hashFile(input) != req.InputSHA256 {
		return empty, errors.New("pdf_scan_input_changed")
	}
	spec.ReadOnlyDirs = nil
	spec.ReadOnlyFiles = []string{output}
	// Give the OCR process its own writable directory. The rendered PNG is
	// mounted as a read-only sibling, and the source PDF is never mounted.
	scratch, err := os.MkdirTemp(filepath.Dir(spec.DataDir), "pdf-ocr-")
	if err != nil {
		return empty, errors.New("pdf_scan_ocr_workspace_failed")
	}
	defer os.RemoveAll(scratch)
	spec.DataDir = scratch
	return RunDocumentOCR(ctx, spec, DocumentOCRRequest{RuntimeRoot: root, InputPath: output, InputSHA256: digest})
}

func verifiedPDFRaster(root string) (string, error) {
	const script = "pdf-raster/render_pdf_page.py"
	const dll = "pdf-raster/pdfium.dll"
	manifest, err := os.Open(filepath.Join(root, "runtime-manifest.json"))
	if err != nil {
		return "", errors.New("pdf_scan_unavailable")
	}
	defer manifest.Close()
	raw, err := io.ReadAll(io.LimitReader(manifest, 8<<20))
	var record struct {
		Components map[string]any    `json:"components"`
		Files      map[string]string `json:"files"`
	}
	if err != nil || json.Unmarshal(raw, &record) != nil || record.Components["pdf_raster"] != pdfRasterVersion ||
		record.Components["tesseract_chi_sim"] != true {
		return "", errors.New("pdf_scan_unavailable")
	}
	scriptSum := sha256.Sum256(trustedPDFRasterScript)
	if record.Files[dll] != pdfiumSHA256 || record.Files[script] != hex.EncodeToString(scriptSum[:]) {
		return "", errors.New("pdf_scan_runtime_changed")
	}
	for _, name := range []string{script, dll} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if !isSHA256(record.Files[name]) || cleanPath(path, false) != nil || hashFile(path) != record.Files[name] {
			return "", errors.New("pdf_scan_runtime_changed")
		}
	}
	return filepath.Join(root, filepath.FromSlash(script)), nil
}

func verifiedPDFRasterImage(path string) (string, error) {
	invalid := errors.New("pdf_scan_invalid_raster")
	if cleanPath(path, false) != nil {
		return "", invalid
	}
	f, err := os.Open(path)
	if err != nil {
		return "", invalid
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() < 1 || info.Size() > maxRasterBytes {
		return "", invalid
	}
	size, err := png.DecodeConfig(f)
	if err != nil || size.Width < 1 || size.Height < 1 || size.Width > 8000 || size.Height > 8000 || size.Width*size.Height > 16_000_000 {
		return "", invalid
	}
	digest := hashFile(path)
	if digest == "" {
		return "", invalid
	}
	return digest, nil
}
