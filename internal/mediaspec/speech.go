package mediaspec

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
)

// GPTSoVITSResult is the JSON returned by the isolated speech runner.
type GPTSoVITSResult struct {
	Output          string  `json:"output"`
	SampleRate      int     `json:"sample_rate"`
	Seconds         float64 `json:"seconds"`
	SHA256          string  `json:"sha256"`
	Device          string  `json:"device"`
	Version         string  `json:"version"`
	GPT             string  `json:"gpt"`
	SoVITS          string  `json:"sovits"`
	GPTSHA256       string  `json:"gpt_sha256"`
	SoVITSSHA256    string  `json:"sovits_sha256"`
	ReferenceSHA256 string  `json:"reference_sha256"`
	PromptSHA256    string  `json:"prompt_sha256"`
	TextSHA256      string  `json:"text_sha256"`
	WeChatSends     int     `json:"wechat_sends"`
}

// SpeechSHA256 returns the hexadecimal digest of a speech artifact.
func SpeechSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PCM16WAVInfo validates the RIFF envelope and returns rate and seconds.
func PCM16WAVInfo(data []byte) (int, float64, error) {
	bad := errors.New("named_speech_wav_invalid")
	if len(data) < 44 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" || int64(binary.LittleEndian.Uint32(data[4:8]))+8 != int64(len(data)) {
		return 0, 0, bad
	}
	var rate, byteRate, block, dataSize uint32
	var haveFormat, haveData bool
	for at := 12; at < len(data); {
		if at+8 > len(data) {
			return 0, 0, bad
		}
		n := int(binary.LittleEndian.Uint32(data[at+4 : at+8]))
		name := string(data[at : at+4])
		at += 8
		if n > len(data)-at {
			return 0, 0, bad
		}
		switch name {
		case "fmt ":
			if haveFormat || n < 16 {
				return 0, 0, bad
			}
			haveFormat = true
			channels := uint32(binary.LittleEndian.Uint16(data[at+2 : at+4]))
			rate = binary.LittleEndian.Uint32(data[at+4 : at+8])
			byteRate = binary.LittleEndian.Uint32(data[at+8 : at+12])
			block = uint32(binary.LittleEndian.Uint16(data[at+12 : at+14]))
			if binary.LittleEndian.Uint16(data[at:at+2]) != 1 || binary.LittleEndian.Uint16(data[at+14:at+16]) != 16 || channels == 0 || channels > 32 || rate == 0 || block != channels*2 || uint64(byteRate) != uint64(rate)*uint64(block) {
				return 0, 0, bad
			}
		case "data":
			if haveData {
				return 0, 0, bad
			}
			haveData = true
			dataSize = uint32(n)
		}
		at += n + (n & 1)
		if at > len(data) {
			return 0, 0, bad
		}
	}
	if !haveFormat || !haveData || dataSize == 0 || block == 0 || dataSize%block != 0 {
		return 0, 0, bad
	}
	return int(rate), float64(dataSize) / float64(byteRate), nil
}
