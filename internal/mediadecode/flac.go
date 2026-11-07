package mediadecode

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// FLACVerificationOptions describes the caller-owned limits for one staged
// audio artifact. FFmpegPath must be an explicit, trusted local executable;
// this package never falls back to PATH for generated audio verification.
type FLACVerificationOptions struct {
	FFmpegPath         string
	MaxInputBytes      int64
	MaxDecodedPCMBytes int64
	MaxSampleRate      uint32
	MaxChannels        uint8
}

// FLACEvidence is evidence about a verified FLAC artifact. Samples is the
// number of decoded PCM frames, rather than an ffprobe estimate.
type FLACEvidence struct {
	SHA256        string
	Bytes         int64
	SampleRate    uint32
	Channels      uint8
	BitsPerSample uint8
	Samples       uint64
	DecodedFrames uint64
	Duration      time.Duration
}

var (
	ErrFLACOptions        = errors.New("flac_verification_options_invalid")
	ErrFLACInput          = errors.New("flac_verification_input_invalid")
	ErrFLACInputLimit     = errors.New("flac_verification_input_limit")
	ErrFLACHeader         = errors.New("flac_verification_header_invalid")
	ErrFLACMetadata       = errors.New("flac_verification_metadata_invalid")
	ErrFLACDecodeLimit    = errors.New("flac_verification_decode_limit")
	ErrFLACDecode         = errors.New("flac_verification_decode_failed")
	ErrFLACSampleMismatch = errors.New("flac_verification_sample_mismatch")
)

// VerifyFLAC validates one already-staged FLAC file without trusting file
// extensions, ffprobe metadata, or partial decodes. It parses STREAMINFO from
// the artifact, hashes the complete input, then decodes the complete stream to
// a bounded PCM byte counter. A truncated or inconsistent stream is rejected.
func VerifyFLAC(ctx context.Context, stagedPath string, options FLACVerificationOptions) (FLACEvidence, error) {
	var evidence FLACEvidence
	if err := validateFLACOptions(options); err != nil {
		return evidence, err
	}
	if !filepath.IsAbs(stagedPath) {
		return evidence, ErrFLACInput
	}
	info, err := os.Lstat(stagedPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 42 {
		return evidence, ErrFLACInput
	}
	if info.Size() > options.MaxInputBytes {
		return evidence, ErrFLACInputLimit
	}

	file, err := os.Open(stagedPath)
	if err != nil {
		return evidence, ErrFLACInput
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() != info.Size() {
		return evidence, ErrFLACInput
	}
	stream, digest, bytesRead, err := readFLACStreamInfoAndHash(file, options.MaxInputBytes)
	if err != nil {
		return evidence, err
	}
	if bytesRead != info.Size() {
		return evidence, ErrFLACInput
	}
	if stream.SampleRate == 0 || stream.SampleRate > options.MaxSampleRate || stream.Channels == 0 || stream.Channels > options.MaxChannels || stream.TotalSamples == 0 {
		return evidence, ErrFLACMetadata
	}
	pcmBytes, err := expectedPCMBytes(stream.TotalSamples, stream.Channels)
	if err != nil || pcmBytes > options.MaxDecodedPCMBytes {
		return evidence, ErrFLACDecodeLimit
	}
	decoded, err := decodeFLACFrames(ctx, file, options.FFmpegPath, pcmBytes)
	if err != nil {
		return evidence, err
	}
	if decoded != pcmBytes {
		return evidence, ErrFLACSampleMismatch
	}
	// The decoder receives this same open handle rather than reopening the path.
	// Hash it again after decode so a concurrent mutation cannot silently turn the
	// pre-decode evidence into evidence for different final bytes.
	_, finalDigest, finalBytes, err := readFLACStreamInfoAndHash(file, options.MaxInputBytes)
	if err != nil || finalBytes != bytesRead || finalDigest != digest {
		return evidence, ErrFLACInput
	}
	frameBytes := int64(stream.Channels) * 2
	if decoded%frameBytes != 0 || uint64(decoded/frameBytes) != stream.TotalSamples {
		return evidence, ErrFLACSampleMismatch
	}

	evidence = FLACEvidence{
		SHA256:        fmt.Sprintf("%x", digest),
		Bytes:         bytesRead,
		SampleRate:    stream.SampleRate,
		Channels:      stream.Channels,
		BitsPerSample: stream.BitsPerSample,
		Samples:       stream.TotalSamples,
		DecodedFrames: uint64(decoded / frameBytes),
		Duration:      durationFromSamples(stream.TotalSamples, stream.SampleRate),
	}
	if evidence.Duration <= 0 {
		return FLACEvidence{}, ErrFLACMetadata
	}
	return evidence, nil
}

type flacStreamInfo struct {
	SampleRate    uint32
	Channels      uint8
	BitsPerSample uint8
	TotalSamples  uint64
}

func validateFLACOptions(options FLACVerificationOptions) error {
	if !filepath.IsAbs(options.FFmpegPath) || options.MaxInputBytes <= 0 || options.MaxDecodedPCMBytes <= 0 || options.MaxSampleRate == 0 || options.MaxChannels == 0 {
		return ErrFLACOptions
	}
	info, err := os.Lstat(options.FFmpegPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrFLACOptions
	}
	return nil
}

func readFLACStreamInfoAndHash(file *os.File, maximum int64) (flacStreamInfo, [sha256.Size]byte, int64, error) {
	var stream flacStreamInfo
	var digest [sha256.Size]byte
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return stream, digest, 0, ErrFLACInput
	}

	var marker [4]byte
	if _, err := io.ReadFull(file, marker[:]); err != nil || string(marker[:]) != "fLaC" {
		return stream, digest, 0, ErrFLACHeader
	}
	var blockHeader [4]byte
	if _, err := io.ReadFull(file, blockHeader[:]); err != nil || blockHeader[0]&0x7f != 0 || int(blockHeader[1])<<16|int(blockHeader[2])<<8|int(blockHeader[3]) != 34 {
		return stream, digest, 0, ErrFLACHeader
	}
	var payload [34]byte
	if _, err := io.ReadFull(file, payload[:]); err != nil {
		return stream, digest, 0, ErrFLACHeader
	}
	stream.SampleRate = uint32(payload[10])<<12 | uint32(payload[11])<<4 | uint32(payload[12])>>4
	stream.Channels = ((payload[12] >> 1) & 0x07) + 1
	stream.BitsPerSample = ((payload[12]&0x01)<<4 | (payload[13] >> 4)) + 1
	stream.TotalSamples = uint64(payload[13]&0x0f)<<32 | uint64(payload[14])<<24 | uint64(payload[15])<<16 | uint64(payload[16])<<8 | uint64(payload[17])

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return stream, digest, 0, ErrFLACInput
	}
	hash := sha256.New()
	bytesRead, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil {
		return stream, digest, bytesRead, ErrFLACInput
	}
	if bytesRead > maximum {
		return stream, digest, bytesRead, ErrFLACInputLimit
	}
	copy(digest[:], hash.Sum(nil))
	return stream, digest, bytesRead, nil
}

func expectedPCMBytes(samples uint64, channels uint8) (int64, error) {
	if samples > uint64(^uint64(0))/uint64(channels) || samples*uint64(channels) > uint64(^uint64(0))/2 {
		return 0, ErrFLACDecodeLimit
	}
	bytes := samples * uint64(channels) * 2
	if bytes > uint64(^uint64(0)>>1) {
		return 0, ErrFLACDecodeLimit
	}
	return int64(bytes), nil
}

func durationFromSamples(samples uint64, sampleRate uint32) time.Duration {
	if sampleRate == 0 || samples > uint64(^uint64(0))/uint64(time.Second) {
		return 0
	}
	return time.Duration(samples*uint64(time.Second)) / time.Duration(sampleRate)
}

func decodeFLACFrames(ctx context.Context, input *os.File, ffmpeg string, maximum int64) (int64, error) {
	output := &flacCountingWriter{maximum: maximum}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return 0, ErrFLACInput
	}
	command := exec.CommandContext(ctx, ffmpeg,
		"-v", "error", "-nostdin", "-max_alloc", "67108864", "-threads", "1",
		"-protocol_whitelist", "pipe", "-f", "flac", "-i", "pipe:0",
		"-map", "0:a:0", "-vn", "-sn", "-dn", "-acodec", "pcm_s16le", "-f", "s16le", "pipe:1",
	)
	hideDecoderWindow(command)
	command.Stdin = input
	command.Stdout = output
	command.Stderr = io.Discard
	err := command.Run()
	if output.exceeded {
		return output.written, ErrFLACDecodeLimit
	}
	if err != nil {
		if ctx.Err() != nil {
			return output.written, ctx.Err()
		}
		return output.written, ErrFLACDecode
	}
	return output.written, nil
}

type flacCountingWriter struct {
	maximum  int64
	written  int64
	exceeded bool
}

func (writer *flacCountingWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.maximum-writer.written {
		writer.exceeded = true
		return 0, ErrFLACDecodeLimit
	}
	writer.written += int64(len(data))
	return len(data), nil
}
