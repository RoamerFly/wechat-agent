package adapter

import (
	"errors"
	"strconv"

	"wechat-bot/internal/mediadecode"
)

// DecodeChannelsPayload is an in-place transformation of host-owned bytes.
// A key is private transfer data; it must never be added to model context.
func DecodeChannelsPayload(data []byte, key string) error {
	if key == "" {
		return nil
	}
	value, err := strconv.ParseUint(key, 10, 64)
	if err != nil {
		return errors.New("channels_decode_key_invalid")
	}
	mediadecode.DecodeChannelsPrefix(data, value)
	return nil
}
