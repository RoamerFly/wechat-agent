package adapter

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAccountOpenIDWireContract(t *testing.T) {
	calls := 0
	c := uploadFixture(t, func(r *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "member.bilibili.com" || r.URL.Path != "/arcopen/fn/user/account/info" || r.URL.RawQuery != "" || len(body) != 0 {
			t.Fatalf("account request shape changed: %s %s %q", r.Method, r.URL, body)
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Access-Token") != "synthetic-oauth" {
			t.Fatal("account request authorization headers changed")
		}
		assertUploadSignature(t, r, body)
		for key := range r.Header {
			if strings.EqualFold(key, "Cookie") {
				t.Fatalf("cookie reached account endpoint: %s", key)
			}
		}
		return uploadResponse(http.StatusOK, `{"code":0,"data":{"openid":"opaque-openid_42-=/+","nickname":"synthetic profile name","face":"https://example.invalid/avatar"}}`), nil
	})
	got, err := c.AccountOpenID(t.Context())
	if err != nil || got != "opaque-openid_42-=/+" || calls != 1 {
		t.Fatalf("AccountOpenID = %q, %v, calls %d", got, err, calls)
	}
}

func TestAccountOpenIDRejectsUnsafeOrMalformedProviderData(t *testing.T) {
	for _, response := range []string{
		`{"code":0,"data":{}}`,
		`{"code":0,"data":{"openid":""}}`,
		`{"code":0,"data":{"openid":"openid with space"}}`,
		`{"code":0,"data":{"openid":"openid\nvalue"}}`,
		`{"code":0,"data":{"openid":"中文"}}`,
		`{"code":0,"data":{"openid":42}}`,
		`{"code":0,"data":{"openid":"one","openid":"two"}}`,
	} {
		c := uploadFixture(t, func(*http.Request) (*http.Response, error) {
			return uploadResponse(http.StatusOK, response), nil
		})
		got, err := c.AccountOpenID(t.Context())
		if !errors.Is(err, ErrOpenUploadResponse) || got != "" || strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), "avatar") {
			t.Fatalf("unsafe provider data accepted or exposed: %q / %v", got, err)
		}
	}
}

func TestAccountOpenIDProviderFailureIsRedacted(t *testing.T) {
	c := uploadFixture(t, func(*http.Request) (*http.Response, error) {
		return uploadResponse(http.StatusOK, `{"code":127001,"message":"synthetic remote credential detail"}`), nil
	})
	got, err := c.AccountOpenID(t.Context())
	var apiErr *OpenAPIError
	if got != "" || !errors.As(err, &apiErr) || apiErr.Kind != "鉴权" || strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("provider failure leaked or was not classified: %q / %v", got, err)
	}
}
