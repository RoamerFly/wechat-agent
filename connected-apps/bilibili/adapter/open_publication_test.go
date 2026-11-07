package adapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOpenArchivePostAndViewWireContract(t *testing.T) {
	const uploadToken = "synthetic-upload-token"
	const resourceID = "BV1aB2cD3e4F"
	calls := 0
	c := uploadFixture(t, func(r *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if r.URL.Host != "member.bilibili.com" || r.Header.Get("Access-Token") != "synthetic-oauth" {
			t.Fatal("archive request lost authorized Open Platform boundary")
		}
		assertUploadSignature(t, r, body)
		switch calls {
		case 1:
			if r.Method != http.MethodPost || r.URL.Path != "/arcopen/fn/archive/add-by-utoken" || r.URL.Query().Get("upload_token") != uploadToken {
				t.Fatal("post target changed")
			}
			want := `{"title":"合成投稿","desc":"离线测试","tid":21,"tag":"生活,测试","copyright":1,"no_reprint":1}`
			if string(body) != want {
				t.Fatalf("post body = %s", body)
			}
			return uploadResponse(http.StatusOK, `{"code":0,"data":{"resource_id":"`+resourceID+`"}}`), nil
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != "/arcopen/fn/archive/view" || r.URL.Query().Get("resource_id") != resourceID || len(body) != 0 {
				t.Fatal("view target changed")
			}
			return uploadResponse(http.StatusOK, `{"code":0,"data":{"resource_id":"`+resourceID+`","addit_info":{"state":0,"state_desc":"开放浏览"}}}`), nil
		default:
			t.Fatalf("unexpected call %d", calls)
			return nil, nil
		}
	})
	resource, err := c.Post(t.Context(), uploadToken, ArchiveMetadata{Title: "合成投稿", Description: "离线测试", CategoryID: 21, Tags: []string{"生活", "测试"}, Copyright: 1, NoReprint: true})
	if err != nil || resource != resourceID {
		t.Fatalf("post = %q, %v", resource, err)
	}
	status, err := c.View(t.Context(), resource)
	if err != nil || !status.Published || calls != 2 {
		t.Fatalf("view = %#v, %v, calls %d", status, err, calls)
	}
}

func TestOpenArchiveValidationAndUnconfirmedStatus(t *testing.T) {
	valid := ArchiveMetadata{Title: "title", CategoryID: 1, Tags: []string{"tag"}, Copyright: 1}
	for _, metadata := range []ArchiveMetadata{
		{}, {Title: "title", CategoryID: 0, Tags: []string{"tag"}, Copyright: 1},
		{Title: "title", CategoryID: 1, Tags: []string{"tag,split"}, Copyright: 1},
		{Title: "title", CategoryID: 1, Tags: []string{"tag"}, Copyright: 3},
		{Title: "title", CategoryID: 1, Tags: []string{"tag"}, Copyright: 2},
	} {
		if !errors.Is(metadata.Validate(), ErrOpenUploadInvalid) {
			t.Fatalf("accepted invalid metadata: %#v", metadata)
		}
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	c := uploadFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("unexpected method")
		}
		return uploadResponse(http.StatusOK, `{"code":0,"data":{"resource_id":"BV1aB2cD3e4F","addit_info":{"state":7,"state_desc":"synthetic remote text"}}}`), nil
	})
	status, err := c.View(context.Background(), "BV1aB2cD3e4F")
	if err != nil || status.Published {
		t.Fatalf("unconfirmed state = %#v, %v", status, err)
	}
	for _, id := range []string{"", "BV1Synthetic42", "BV1aB2cD3e4-", " id", "id\nvalue"} {
		if _, err := c.View(t.Context(), id); !errors.Is(err, ErrOpenUploadInvalid) {
			t.Fatalf("accepted unsafe resource id %q: %v", id, err)
		}
	}
}

func TestOpenArchiveResponseErrorsAreRedactedAndIDMustMatch(t *testing.T) {
	for _, response := range []string{
		`{"code":0,"data":{"resource_id":"BV1aB2cD3e4G","addit_info":{"state":0,"state_desc":"开放浏览"}}}`,
		`{"code":0,"data":{"resource_id":"BV1aB2cD3e4F","addit_info":{}}}`,
		`{"code":0,"data":{"resource_id":"BV1aB2cD3e4F"}}`,
	} {
		c := uploadFixture(t, func(*http.Request) (*http.Response, error) { return uploadResponse(http.StatusOK, response), nil })
		_, err := c.View(t.Context(), "BV1aB2cD3e4F")
		if !errors.Is(err, ErrOpenUploadResponse) || strings.Contains(err.Error(), "BV1aB2cD3e4G") {
			t.Fatalf("unsafe/missing response rejected incorrectly: %v", err)
		}
	}
}

func TestOpenArchivePostRejectsNonCanonicalProviderID(t *testing.T) {
	c := uploadFixture(t, func(*http.Request) (*http.Response, error) {
		return uploadResponse(http.StatusOK, `{"code":0,"data":{"resource_id":"synthetic provider text"}}`), nil
	})
	_, err := c.Post(t.Context(), "synthetic-upload-token", ArchiveMetadata{Title: "title", CategoryID: 1, Tags: []string{"tag"}, Copyright: 1})
	if !errors.Is(err, ErrOpenUploadResponse) || strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("unsafe post response was exposed: %v", err)
	}
}
