package adapter

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestOpenArchiveListWireContractAndPage(t *testing.T) {
	calls := 0
	c := uploadFixture(t, func(request *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "member.bilibili.com" || request.URL.Path != "/arcopen/fn/archive/viewlist" || request.URL.Query().Get("pn") != "2" || request.URL.Query().Get("ps") != "50" || request.URL.Query().Get("status") != "pubed" || len(body) != 0 {
			t.Fatalf("list request changed: %s %s %q", request.Method, request.URL, body)
		}
		if request.Header.Get("Accept") != "application/json" || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Access-Token") != "synthetic-oauth" {
			t.Fatal("list authorization headers changed")
		}
		assertUploadSignature(t, request, body)
		return uploadResponse(http.StatusOK, `{"code":0,"data":{"list":[{"resource_id":"BV1aB2cD3e4F","title":"已确认候选","ctime":1700000000,"ptime":1700000010,"addit_info":{"state":0,"state_desc":"开放浏览"}}],"page":{"pn":2,"ps":50,"total":101}}}`), nil
	})
	page, err := c.ListArchives(t.Context(), ArchiveListRequest{Page: 2, PageSize: 50, Status: "pubed"})
	if err != nil || calls != 1 || page.Page != 2 || page.PageSize != 50 || page.Total != 101 || page.NextPage != 3 || len(page.Items) != 1 || page.Items[0].ResourceID != "BV1aB2cD3e4F" || page.Items[0].CreatedAt == nil || *page.Items[0].CreatedAt != 1700000000 {
		t.Fatalf("ListArchives = %#v, %v, calls %d", page, err, calls)
	}
}

func TestOpenArchiveListDefaultsAndValidationNeverReachTransport(t *testing.T) {
	calls := 0
	c := uploadFixture(t, func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Query().Get("pn") != "1" || request.URL.Query().Get("ps") != "20" || request.URL.Query().Get("status") != "" {
			t.Fatalf("default page not effective: %s", request.URL.RawQuery)
		}
		return uploadResponse(http.StatusOK, `{"code":0,"data":{"list":[],"page":{"pn":1,"ps":20,"total":0}}}`), nil
	})
	if page, err := c.ListArchives(t.Context(), ArchiveListRequest{}); err != nil || page.NextPage != 0 || calls != 1 {
		t.Fatalf("default ListArchives = %#v, %v, calls %d", page, err, calls)
	}
	for _, input := range []ArchiveListRequest{{Page: -1}, {PageSize: 51}, {PageSize: -1}, {Status: "published"}} {
		if _, err := c.ListArchives(t.Context(), input); !errors.Is(err, ErrOpenUploadInvalid) {
			t.Fatalf("accepted invalid list request %#v: %v", input, err)
		}
	}
	if calls != 1 {
		t.Fatalf("invalid list request reached transport %d times", calls)
	}
}

func TestOpenArchiveListAllowsNullEmptyListOnlyForZeroTotal(t *testing.T) {
	c := uploadFixture(t, func(*http.Request) (*http.Response, error) {
		return uploadResponse(http.StatusOK, `{"code":0,"data":{"list":null,"page":{"pn":1,"ps":20,"total":0}}}`), nil
	})
	page, err := c.ListArchives(t.Context(), ArchiveListRequest{})
	if err != nil || len(page.Items) != 0 || page.NextPage != 0 {
		t.Fatalf("null empty list = %#v / %v", page, err)
	}
}

func TestOpenArchiveListPaginationDoesNotMultiplyLargePageValues(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	data := []byte(`{"list":[],"page":{"pn":` + strconv.Itoa(maxInt) + `,"ps":1,"total":` + strconv.Itoa(maxInt) + `}}`)
	page, err := decodeArchiveList(data, ArchiveListRequest{Page: maxInt, PageSize: 1})
	if err != nil || page.NextPage != 0 || page.Total != maxInt {
		t.Fatalf("large pagination = %#v / %v", page, err)
	}
}

func TestOpenArchiveListRejectsMalformedProviderMetadata(t *testing.T) {
	responses := []string{
		`{}`,
		`{"list":[],"page":{"pn":2,"ps":20,"total":0}}`,
		`{"list":[],"page":{"pn":1,"ps":20,"total":-1}}`,
		`{"list":[{"resource_id":"not-a-bv","title":"title","ctime":0,"ptime":0,"addit_info":{"state":0,"state_desc":"开放浏览"}}],"page":{"pn":1,"ps":20,"total":1}}`,
		`{"list":[{"resource_id":"BV1aB2cD3e4F","title":"title\nunsafe","ctime":0,"ptime":0,"addit_info":{"state":0,"state_desc":"开放浏览"}}],"page":{"pn":1,"ps":20,"total":1}}`,
		`{"list":[{"resource_id":"BV1aB2cD3e4F","title":"title","ctime":-1}],"page":{"pn":1,"ps":20,"total":1}}`,
	}
	for index, data := range responses {
		c := uploadFixture(t, func(*http.Request) (*http.Response, error) {
			return uploadResponse(http.StatusOK, `{"code":0,"data":`+data+`}`), nil
		})
		page, err := c.ListArchives(t.Context(), ArchiveListRequest{})
		if !errors.Is(err, ErrOpenUploadResponse) || len(page.Items) != 0 || strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("unsafe list response %d = %#v / %v", index, page, err)
		}
	}
}
