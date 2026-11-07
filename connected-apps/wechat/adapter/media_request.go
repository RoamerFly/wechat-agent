package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"
)

type VoiceConversion struct {
	StartSeconds    float64 `json:"start_seconds,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	SegmentSeconds  float64 `json:"segment_seconds,omitempty"`
	TimeoutSeconds  float64 `json:"timeout_seconds,omitempty"`
}

func (c *VoiceConversion) Effective() VoiceConversion {
	v := VoiceConversion{SegmentSeconds: 60, TimeoutSeconds: 90}
	if c != nil {
		v.StartSeconds, v.DurationSeconds = c.StartSeconds, c.DurationSeconds
		if c.SegmentSeconds != 0 {
			v.SegmentSeconds = c.SegmentSeconds
		}
		if c.TimeoutSeconds != 0 {
			v.TimeoutSeconds = c.TimeoutSeconds
		}
	}
	return v
}

func (c *VoiceConversion) Validate(kind string) error {
	if c == nil {
		return nil
	}
	if kind != "voice" {
		return errors.New("conversion is supported for kind=voice only; file keeps original bytes")
	}
	for _, n := range []float64{c.StartSeconds, c.DurationSeconds, c.SegmentSeconds, c.TimeoutSeconds} {
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return errors.New("conversion seconds must be finite and nonnegative")
		}
	}
	v := c.Effective()
	if v.SegmentSeconds > 60 {
		return errors.New("WeChat voice protocol supports at most 60 seconds per part; choose <=60 and send all parts in order")
	}
	if v.TimeoutSeconds >= float64(math.MaxInt64)/float64(time.Second) || v.TimeoutSeconds < 0.001 {
		return errors.New("timeout_seconds exceeds timer precision/range")
	}
	return nil
}

type MediaRequest struct {
	Conversion *VoiceConversion `json:"conversion,omitempty"`
	Source     string           `json:"source_thread_id"`
	Target     string           `json:"target_thread_id"`
	Asset      string           `json:"asset_id"`
	Message    string           `json:"message_id"`
	Kind       string           `json:"kind"`
	Key        string           `json:"request_key"`
}

func ParseMediaRequest(raw json.RawMessage, current string) (MediaRequest, error) {
	var in MediaRequest
	if json.Unmarshal(raw, &in) != nil || in.Key == "" || len(in.Key) > 128 || (in.Asset == "") == (in.Message == "") || !contains([]string{"file", "voice", "video", "image"}, in.Kind) {
		return in, errors.New("provide one real asset_id or message_id, kind and stable request_key")
	}
	if err := in.Conversion.Validate(in.Kind); err != nil {
		return in, err
	}
	if in.Source == "" {
		in.Source = current
	}
	if in.Target == "" {
		in.Target = current
	}
	return in, nil
}

// MediaSession is issued for one host-verified request. All assets, source
// evidence, staging and canonical queue entries remain inside the host.
type MediaSession interface {
	Resolve(context.Context) error
	Existing(context.Context) (any, bool, error)
	Prepare(context.Context) error
	Queue(context.Context) (any, error)
}
type MediaHost interface {
	CurrentThread() string
	Invalid(string) error
	Authorize(context.Context, MediaRequest) error
	ClaimMedia(context.Context, MediaRequest, func(MediaSession) (any, error)) (string, error)
}

func SendMedia(ctx context.Context, raw json.RawMessage, host MediaHost) (string, error) {
	in, err := ParseMediaRequest(raw, host.CurrentThread())
	if err != nil {
		return "", host.Invalid(err.Error())
	}
	if err = host.Authorize(ctx, in); err != nil {
		return "", err
	}
	return host.ClaimMedia(ctx, in, func(session MediaSession) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := session.Resolve(ctx); err != nil {
			return nil, err
		}
		if result, exists, err := session.Existing(ctx); err != nil || exists {
			return result, err
		}
		if err := session.Prepare(ctx); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return session.Queue(ctx)
	})
}
