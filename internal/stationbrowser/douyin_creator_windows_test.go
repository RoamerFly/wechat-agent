//go:build windows

package stationbrowser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreatorReceiptIDsRemainExactAndUnambiguous(t *testing.T) {
	for _, tc := range []struct{ raw, id string }{
		{`{"status_code":0,"aweme_id":7391234567890123456}`, "7391234567890123456"},
		{`{"status_code":0,"aweme":{"aweme_id":"123"}}`, "123"},
		{`{"status_code":0,"data":{"aweme_id":"123"}}`, "123"},
		{`{"status_code":0,"item_id":"7391234567890123456"}`, "7391234567890123456"},
		{`{"status_code":0,"item_id":"123","aweme_id":"456"}`, ""},
		{`{"status_code":1,"aweme_id":"123"}`, ""},
		{`{"aweme_id":"123"}`, ""},
		{`{"status_code":0,"aweme_id":"123","data":{"aweme_id":"456"}}`, ""},
		{`{"status_code":0,"aweme_id":"https://example.invalid"}`, ""},
	} {
		if got := creatorReceiptID([]byte(tc.raw)); got != tc.id {
			t.Fatalf("receipt %s: %q", tc.raw, got)
		}
	}
	l := newCreatorReceiptListener(douyinCreatorUpload, "session")
	for _, endpoint := range []string{"https://creator.douyin.com.evil.invalid/web/api/media/aweme/create/", "https://creator.douyin.com/other", "https://user@creator.douyin.com/web/api/media/aweme/create/"} {
		if l.endpoint(endpoint) {
			t.Fatalf("unexpected endpoint accepted: %s", endpoint)
		}
	}
}

func TestCreatorDependenciesStaySeparateFromOrdinaryFeed(t *testing.T) {
	for _, host := range []string{"lf-fe-creator.douyinstatic.com", "auth.zijieapi.com", "lf1-cdn-tos.bytescm.com"} {
		if !isDouyinCreatorHost(host) || isDouyinHomeHost(host) {
			t.Fatalf("creator dependency %q not independently scoped", host)
		}
	}
	for _, host := range []string{"lf-fe-creator.douyinstatic.com.evil.invalid", "127.0.0.1", "example.invalid", "user@auth.zijieapi.com"} {
		if isDouyinCreatorHost(host) {
			t.Fatalf("unexpected creator host %q", host)
		}
	}
}

func creatorFixture(t *testing.T, reject, stale bool) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	posts, uploads := new(atomic.Int32), new(atomic.Int32)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/creator-micro/content/upload":
			uploads.Add(1)
			fmt.Fprint(w, `<input type=file accept="image/*"><input id=video type=file accept="video/*"><input placeholder="标题"><textarea placeholder="描述"></textarea><label><input type=radio name=v value=private>仅自己可见</label><label><input type=radio name=v value=public>公开</label><button disabled>发布</button><script>
video.onchange=()=>{setTimeout(()=>document.querySelector('button').disabled=false,150)};
document.querySelector('button').onclick=()=>{
 const title=document.querySelector('[placeholder="标题"]').value;
 const body=document.querySelector('textarea').value;
 const privacy=document.querySelector('input[name=v]:checked')?.value;
 if(title==='合成标题' && body==='测试内容' && privacy==='private' && video.files[0]?.name==='playback.webm') fetch('/web/api/media/aweme/create/',{method:'POST'});
};</script>`)
			if stale {
				fmt.Fprint(w, `<script>fetch('/web/api/media/aweme/create/?stale=1',{method:'POST'})</script>`)
			}
			if r.URL.Query().Get("clear") == "1" {
				fmt.Fprint(w, `<script>document.querySelector('[placeholder="标题"]').addEventListener('input',()=>video.value='')</script>`)
			}
			if r.URL.Query().Get("spa") == "1" {
				fmt.Fprint(w, `<script>
const originalUpload=video;
const editor=document.createElement('div');editor.contentEditable='true';editor.dataset.placeholder='添加作品简介';editor.style.minHeight='30px';document.querySelector('textarea').replaceWith(editor);
editor.addEventListener('input',()=>{editor.removeAttribute('data-placeholder');if(!editor.textContent.endsWith('\u200b'))editor.append(document.createTextNode('\u200b'))});
for(const label of document.querySelectorAll('label')){label.dataset.checked='false';label.querySelector('input').type='checkbox';label.onclick=()=>{for(const other of document.querySelectorAll('label')){other.dataset.checked=String(other===label);other.querySelector('input').checked=other===label;}}}
originalUpload.onchange=()=>setTimeout(()=>{const preview=document.createElement('video');preview.src='/clip.webm';preview.muted=true;preview.autoplay=true;preview.style.width='240px';document.body.append(preview);originalUpload.remove();document.querySelector('button').disabled=false;},150);
document.querySelector('button').onclick=()=>{if(editor.innerText.replace(/\u200b/g,'')==='测试内容'&&document.querySelector('label[data-checked=true] input')?.value==='private'&&originalUpload.files[0]?.name==='playback.webm')fetch('/web/api/media/aweme/create/',{method:'POST'})};
</script>`)
			}
		case "/clip.webm":
			data, err := os.ReadFile(filepath.Join("testdata", "playback.webm"))
			if err != nil {
				t.Error(err)
				http.Error(w, "missing fixture", 500)
				return
			}
			w.Header().Set("Content-Type", "video/webm")
			http.ServeContent(w, r, "clip.webm", time.Time{}, bytes.NewReader(data))
		case "/creator-micro/content/manage":
			fmt.Fprint(w, `<article data-e2e="work-item"><a href="https://www.douyin.com/video/7391234567890123456">合成标题</a> 审核中 </article><p>另一条已发布</p>`)
		case "/web/api/media/aweme/create/":
			posts.Add(1)
			if r.URL.Query().Get("stale") == "1" {
				fmt.Fprint(w, `{"status_code":0,"aweme_id":"111"}`)
				return
			}
			if reject {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"status_code":42,"status_msg":"synthetic rejection"}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status_code":0,"aweme_id":7391234567890123456}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s, posts, uploads
}

func TestLocalCreatorSPAUploadTransitionAndRichText(t *testing.T) {
	if !Available() {
		t.Skip("Edge unavailable")
	}
	s, posts, _ := creatorFixture(t, false, false)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	in := creatorRequest(t)
	in.BeforeSubmit = func(context.Context) error { return nil }
	result, err := runDouyinCreator(ctx, filepath.Join(t.TempDir(), "profile"), s.URL+"/creator-micro/content/upload?spa=1", in, func() error { return nil }, true)
	if err != nil || result.Stage != "accepted" || posts.Load() != 1 {
		t.Fatalf("SPA transition %#v %v posts=%d", result, err, posts.Load())
	}
}

func creatorRequest(t *testing.T) CreatorPublicationRequest {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "playback.webm"))
	if err != nil {
		t.Fatal(err)
	}
	return CreatorPublicationRequest{TaskID: "synthetic-job", ManifestSHA256: strings.Repeat("a", 64), Title: "合成标题", Body: "测试内容", Visibility: "private", MediaPath: path}
}

func TestLocalCreatorUploadCheckpointSubmitAndQuery(t *testing.T) {
	if !Available() {
		t.Skip("Edge unavailable")
	}
	s, posts, uploads := creatorFixture(t, false, true)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	in := creatorRequest(t)
	checkpoints := 0
	in.BeforeSubmit = func(context.Context) error {
		if posts.Load() != 1 {
			t.Fatal("submit before checkpoint")
		}
		checkpoints++
		return nil
	}
	profile := filepath.Join(t.TempDir(), "profile")
	result, err := runDouyinCreator(ctx, profile, s.URL+"/creator-micro/content/upload", in, func() error { return nil }, true)
	if err != nil || !result.Submitted || result.Published || result.RemoteID != "7391234567890123456" || posts.Load() != 2 || checkpoints != 1 {
		t.Fatalf("upload %#v %v posts=%d checkpoints=%d", result, err, posts.Load(), checkpoints)
	}
	in.Submitted, in.RemoteID = true, result.RemoteID
	result, err = runDouyinCreator(ctx, profile, s.URL+"/creator-micro/content/manage", in, func() error { return nil }, true)
	if err != nil || result.Published || result.Stage != "accepted" || uploads.Load() != 1 || posts.Load() != 2 {
		t.Fatalf("query %#v %v uploads=%d posts=%d", result, err, uploads.Load(), posts.Load())
	}
}

func TestLocalCreatorDurableFailureAndCancellationPreventSubmit(t *testing.T) {
	if !Available() {
		t.Skip("Edge unavailable")
	}
	s, posts, _ := creatorFixture(t, false, false)
	for _, cancelAfterCheckpoint := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		in := creatorRequest(t)
		revoked := false
		in.BeforeSubmit = func(context.Context) error {
			if cancelAfterCheckpoint {
				revoked = true
				return nil
			}
			return errors.New("synthetic disk failure")
		}
		result, err := runDouyinCreator(ctx, filepath.Join(t.TempDir(), "profile"), s.URL+"/creator-micro/content/upload", in, func() error {
			if revoked {
				return context.Canceled
			}
			return nil
		}, true)
		cancel()
		if err == nil || result.Submitted != cancelAfterCheckpoint || posts.Load() != 0 {
			t.Fatalf("unsafe checkpoint/cancel: %#v %v posts=%d", result, err, posts.Load())
		}
	}
}

func TestCreatorSubmittedWithoutIDDoesNotOpenBrowserOrRepeat(t *testing.T) {
	result, err := ContinueDouyinPublication(t.Context(), "unused", CreatorPublicationRequest{Submitted: true}, func() error { t.Fatal("unknown receipt opened browser"); return nil })
	if !errors.Is(err, ErrCreatorPublicationUnknown) || !result.Submitted {
		t.Fatalf("unknown state %#v %v", result, err)
	}
}

func TestLocalCreatorExplicitRejectionIsNotUnknownTimeout(t *testing.T) {
	if !Available() {
		t.Skip("Edge unavailable")
	}
	s, posts, _ := creatorFixture(t, true, false)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	in := creatorRequest(t)
	in.BeforeSubmit = func(context.Context) error { return nil }
	result, err := runDouyinCreator(ctx, filepath.Join(t.TempDir(), "profile"), s.URL+"/creator-micro/content/upload", in, func() error { return nil }, true)
	if !errors.Is(err, ErrCreatorPublicationRejected) || !result.Submitted || result.Stage != "rejected" || result.RejectionCode != 42 || result.RemoteID != "" || posts.Load() != 1 {
		t.Fatalf("explicit rejection %#v %v posts=%d", result, err, posts.Load())
	}
}

func TestLocalCreatorClearedFileNeverEntersSubmit(t *testing.T) {
	if !Available() {
		t.Skip("Edge unavailable")
	}
	s, posts, _ := creatorFixture(t, false, false)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	in := creatorRequest(t)
	in.BeforeSubmit = func(context.Context) error { t.Fatal("cleared input crossed submit boundary"); return nil }
	result, err := runDouyinCreator(ctx, filepath.Join(t.TempDir(), "profile"), s.URL+"/creator-micro/content/upload?clear=1", in, func() error { return nil }, true)
	if !errors.Is(err, ErrCreatorPublicationUpload) || result.Submitted || posts.Load() != 0 {
		t.Fatalf("cleared file: %#v %v posts=%d", result, err, posts.Load())
	}
}
