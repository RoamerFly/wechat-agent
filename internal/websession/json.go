package websession

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

func Respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func Fail(w http.ResponseWriter, status int, message string) {
	Respond(w, status, map[string]string{"error": message})
}

func Decode(w http.ResponseWriter, r *http.Request, value any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		Fail(w, http.StatusUnsupportedMediaType, "请求需要使用 JSON")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(value) != nil {
		Fail(w, http.StatusBadRequest, "请求格式无效或过大")
		return false
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		Fail(w, http.StatusBadRequest, "请求需要一个完整 JSON 对象")
		return false
	}
	return true
}
