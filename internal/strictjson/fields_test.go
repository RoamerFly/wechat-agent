package strictjson

import "testing"

func TestDecodeKnownFieldsRejectsUnknownAndTrailingValues(t *testing.T) {
	var got struct {
		State string `json:"state"`
	}
	if err := DecodeKnownFields([]byte(`{"state":"completed"} `), &got); err != nil || got.State != "completed" {
		t.Fatalf("valid tool result was rejected: %q %v", got.State, err)
	}
	for _, raw := range []string{
		`{"state":"completed","other":true}`,
		`{"state":"completed"} {"state":"forged"}`,
	} {
		if err := DecodeKnownFields([]byte(raw), &got); err == nil {
			t.Fatalf("unsafe tool result was accepted: %s", raw)
		}
	}
}
