package stationyue

import "errors"

// DurationCheck describes post-render acceptance only. It never trims or
// alters audio. Codec/container reporting may overshoot by up to 0.5 seconds;
// an optional frame precision extends that upper tolerance by one frame.
type DurationCheck struct {
	ActualSeconds, RequestedSeconds, MinimumSeconds, UpperBoundSeconds float64
	Accepted                                                           bool
}

func ValidateDuration(actual, requested, minimum float64, streamFramePrecision *float64) (DurationCheck, error) {
	if !finite(actual) || !finite(requested) || !finite(minimum) || actual < 0 || requested <= 0 || minimum < 0 || minimum > requested {
		return DurationCheck{}, errors.New("stationyue: invalid_duration_measurement")
	}
	tolerance := .5
	if streamFramePrecision != nil {
		if !finite(*streamFramePrecision) || *streamFramePrecision <= 0 || *streamFramePrecision > 1 {
			return DurationCheck{}, errors.New("stationyue: invalid_stream_frame_precision")
		}
		tolerance += *streamFramePrecision
	}
	upper := requested + tolerance
	return DurationCheck{actual, requested, minimum, upper, actual > 0 && actual >= minimum && actual <= upper}, nil
}
