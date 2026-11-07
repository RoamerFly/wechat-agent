package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/pluginhost"
)

// These tests deliberately exercise the host publisher instead of calling the
// adapter workflow directly.  That is the boundary which owns the durable
// ledger, cancellation ordering and the safe owner-visible task view.
type douyinPublicationFixture struct {
	t       *testing.T
	app     *SearchApp
	store   *pluginhost.Store
	publish *apphost.Publisher
	actor   pluginhost.Invocation
	draft   apphost.PublicationDraft
	media   []byte
	dir     string
}

func newDouyinPublicationFixture(t *testing.T, media []byte, transport roundTrip) *douyinPublicationFixture {
	t.Helper()
	app := testPublicationApp(t, transport)
	account := accountAuthorization{
		SchemaVersion: 1, Revision: strings.Repeat("a", 48), ClientKey: "fixture-client", OpenID: "fixture_open_id",
		AccessToken: "act.fixture-publish-token", Label: "合成抖音账号", PublishVideo: true,
		ExpiresAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := app.writeAccount(account); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(media)
	f := &douyinPublicationFixture{
		t: t, app: app, dir: t.TempDir(), media: media,
		actor: pluginhost.Invocation{Account: "fish-fixture", Thread: "owner-fixture", Requester: "owner-fixture", Purpose: "synthetic publication", RequestKey: "publication-fixture", Authorization: "home-fixture"},
	}
	store, err := pluginhost.OpenStore(f.dir, f.actor.Account)
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	t.Cleanup(func() {
		if f.store != nil {
			_ = f.store.Close()
		}
	})
	registry, err := apphost.New(app.Registration())
	if err != nil {
		t.Fatal(err)
	}
	target, err := apphost.NewPublisher(registry, store, nil, nil).ResolveTarget(t.Context(), ID, accountConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	f.draft = apphost.PublicationDraft{
		Target: target, Title: "合成发布标题", Body: "合成发布说明", Options: json.RawMessage(`{"visibility":"public"}`), Origin: json.RawMessage(`{"source":"synthetic"}`),
		Artifacts: []apphost.PublicationArtifact{{ID: "artifact-fixture", SHA256: hex.EncodeToString(hash[:]), Name: "fixture.mp4", Bytes: int64(len(media))}},
	}
	f.publish = apphost.NewPublisher(registry, store, nil, func(_ context.Context, artifact apphost.PublicationArtifact) (io.ReadCloser, error) {
		if artifact != f.draft.Artifacts[0] {
			return nil, errors.New("unexpected artifact")
		}
		return io.NopCloser(bytes.NewReader(f.media)), nil
	})
	return f
}

func (f *douyinPublicationFixture) restart(transport roundTrip) {
	f.t.Helper()
	if err := f.store.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.app = New(f.app.privateDirectory)
	f.app.now = func() time.Time { return time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC) }
	f.app.client = &http.Client{Transport: transport}
	store, err := pluginhost.OpenStore(f.dir, f.actor.Account)
	if err != nil {
		f.t.Fatal(err)
	}
	f.store = store
	registry, err := apphost.New(f.app.Registration())
	if err != nil {
		f.t.Fatal(err)
	}
	f.publish = apphost.NewPublisher(registry, store, nil, func(_ context.Context, artifact apphost.PublicationArtifact) (io.ReadCloser, error) {
		if artifact != f.draft.Artifacts[0] {
			return nil, errors.New("unexpected artifact")
		}
		return io.NopCloser(bytes.NewReader(f.media)), nil
	})
}

func (f *douyinPublicationFixture) save() string {
	f.t.Helper()
	view, err := f.publish.Draft(f.t.Context(), f.actor, f.draft)
	if err != nil || view.State != "draft" {
		f.t.Fatalf("draft: %+v %v", view, err)
	}
	return view.JobID
}

func (f *douyinPublicationFixture) action(id, action, state string) apphost.PublicationView {
	f.t.Helper()
	view, err := f.publish.Action(f.t.Context(), f.actor, id, action)
	if err != nil || view.State != state {
		f.t.Fatalf("%s: %+v %v; want %s", action, view, err, state)
	}
	return view
}

func fixtureVideo(size int) []byte {
	if size < 12 {
		size = 12
	}
	media := bytes.Repeat([]byte{0x5a}, size)
	copy(media, []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'})
	return media
}

func TestDouyinPublicationPublishesOnlyAfterExplicitStatusAndKeepsOpaqueItemPrivate(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	transport := roundTrip(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, request.URL.Path)
		if request.Header.Get("access-token") != "act.fixture-publish-token" || request.URL.Query().Get("open_id") != "fixture_open_id" {
			t.Fatalf("publication used wrong account credentials: %s %v", request.URL, request.Header)
		}
		switch request.URL.Path {
		case "/api/douyin/v1/video/upload_video/":
			return jsonResponse(`{"data":{"error_code":0,"video":{"video_id":"upload-token"}},"extra":{"error_code":0}}`), nil
		case "/api/douyin/v1/video/create_video/":
			return jsonResponse(`{"data":{"error_code":0,"video_id":"7000000000000000001","item_id":"private-item-do-not-display"},"extra":{"error_code":0}}`), nil
		case "/api/douyin/v1/video/video_basic_info/":
			return jsonResponse(`{"data":{"error_code":0,"list":[{"video_id":"7000000000000000001","video_status":5}]},"extra":{"error_code":0}}`), nil
		default:
			t.Fatalf("unexpected endpoint %s", request.URL.Path)
			return nil, nil
		}
	})
	f := newDouyinPublicationFixture(t, fixtureVideo(128), transport)
	if targets := f.publish.Discover(t.Context()); !reflect.DeepEqual(targets, []apphost.ConnectionRef{f.draft.Target}) {
		t.Fatalf("discover=%+v, want authorized publication account only", targets)
	}
	id := f.save()
	accepted := f.action(id, "submit", "accepted")
	if accepted.RemoteID != "7000000000000000001" || accepted.Progress == nil || accepted.Progress.Stage != "accepted" {
		t.Fatalf("accepted view=%+v", accepted)
	}
	encoded, _ := json.Marshal(accepted)
	if strings.Contains(string(encoded), "private-item-do-not-display") || strings.Contains(string(encoded), "act.fixture") {
		t.Fatalf("private provider values leaked in workbench view: %s", encoded)
	}
	published := f.action(id, "status", "published")
	if published.RemoteID != accepted.RemoteID || !reflect.DeepEqual(calls, []string{
		"/api/douyin/v1/video/upload_video/", "/api/douyin/v1/video/create_video/", "/api/douyin/v1/video/video_basic_info/",
	}) {
		t.Fatalf("published=%+v calls=%v", published, calls)
	}
}

func TestDouyinPublicationAmbiguousCreateNeverReplaysAcrossRestartAndCancelStaysPending(t *testing.T) {
	var mu sync.Mutex
	createCalls := 0
	transport := roundTrip(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		switch request.URL.Path {
		case "/api/douyin/v1/video/upload_video/":
			return jsonResponse(`{"data":{"error_code":0,"video":{"video_id":"upload-token"}},"extra":{"error_code":0}}`), nil
		case "/api/douyin/v1/video/create_video/":
			createCalls++
			return nil, errors.New("synthetic connection loss after create request")
		default:
			t.Fatalf("unexpected endpoint %s", request.URL.Path)
			return nil, nil
		}
	})
	f := newDouyinPublicationFixture(t, fixtureVideo(128), transport)
	id := f.save()
	first := f.action(id, "submit", "awaiting_receipt")
	if first.Progress == nil || first.Progress.Stage != "posting" || createCalls != 1 {
		t.Fatalf("ambiguous create was not durably claimed: %+v calls=%d", first, createCalls)
	}
	f.restart(transport)
	f.action(id, "status", "awaiting_receipt")
	stopped := f.action(id, "cancel", "awaiting_receipt")
	if !stopped.CancelRequested || createCalls != 1 {
		t.Fatalf("unknown create was replayed or terminally cancelled: %+v create=%d", stopped, createCalls)
	}
}

func TestDouyinPublicationCanCancelDraftBeforeAnyProviderCreation(t *testing.T) {
	providerCalls := 0
	f := newDouyinPublicationFixture(t, fixtureVideo(128), roundTrip(func(request *http.Request) (*http.Response, error) {
		providerCalls++
		t.Fatalf("draft cancellation must not call provider: %s", request.URL)
		return nil, nil
	}))
	id := f.save()
	stopped := f.action(id, "cancel", "cancelled")
	if !stopped.CancelRequested || providerCalls != 0 {
		t.Fatalf("cancelled=%+v providerCalls=%d", stopped, providerCalls)
	}
}

type publicationBudgetFixture struct {
	context.Context
	yield *atomic.Bool
}

func (c publicationBudgetFixture) Deadline() (time.Time, bool) {
	if c.yield.Load() {
		return time.Now().Add(time.Second), true
	}
	return c.Context.Deadline()
}

func TestDouyinPublicationOnlyResumesConfirmedPartAndNeverReplaysUncertainPart(t *testing.T) {
	media := fixtureVideo(douyinSingleUploadBytes + 1)
	for _, interrupted := range []bool{false, true} {
		name := "known-boundary-after-renewal"
		if interrupted {
			name = "uncertain-part"
		}
		t.Run(name, func(t *testing.T) {
			var yield atomic.Bool
			var parts []string
			wantToken := "act.fixture-publish-token"
			transport := roundTrip(func(request *http.Request) (*http.Response, error) {
				if strings.HasPrefix(request.URL.Path, "/api/") && request.Header.Get("access-token") != wantToken {
					t.Fatal("publication reused stale account credentials")
				}
				switch request.URL.Path {
				case "/oauth/refresh_token/":
					return jsonResponse(`{"data":{"error_code":0,"open_id":"fixture_open_id","scope":"user_info,video.create.bind,posting.behavior","access_token":"act.fixture-renewed-token","refresh_token":"rft.fixture-token","expires_in":3600,"refresh_expires_in":7200}}`), nil
				case "/oauth/userinfo/":
					return jsonResponse(`{"data":{"error_code":"0","open_id":"fixture_open_id","nickname":"合成抖音账号"}}`), nil
				case "/api/douyin/v1/video/init_video_part_upload/":
					return jsonResponse(`{"data":{"error_code":0,"upload_id":"upload-token"},"extra":{"error_code":0}}`), nil
				case "/api/douyin/v1/video/upload_video_part/":
					part := request.URL.Query().Get("part_number")
					parts = append(parts, part)
					if interrupted && part == "2" {
						return nil, errors.New("synthetic disconnect during part 2")
					}
					if !interrupted && part == "1" {
						yield.Store(true)
					}
					return jsonResponse(`{"data":{"error_code":0},"extra":{"error_code":0}}`), nil
				case "/api/douyin/v1/video/complete_video_part_upload/":
					return jsonResponse(`{"data":{"error_code":0,"video_id":"upload-token"},"extra":{"error_code":0}}`), nil
				case "/api/douyin/v1/video/create_video/":
					return jsonResponse(`{"data":{"error_code":0,"video_id":"7000000000000000002","item_id":"private-resume-item"},"extra":{"error_code":0}}`), nil
				default:
					t.Fatalf("unexpected endpoint %s", request.URL.Path)
					return nil, nil
				}
			})
			f := newDouyinPublicationFixture(t, media, transport)
			id := f.save()
			ctx := publicationBudgetFixture{Context: t.Context(), yield: &yield}
			first, err := f.publish.Action(ctx, f.actor, id, "submit")
			if err != nil || first.State != "awaiting_receipt" || first.Progress == nil || first.Progress.ConfirmedParts != 1 || first.Progress.TotalParts != 3 {
				t.Fatalf("first part progress=%+v, err=%v", first, err)
			}
			f.restart(transport)
			if interrupted {
				if first.Progress.Stage != "part_pending" {
					t.Fatalf("ambiguous part did not retain its claim: %+v", first.Progress)
				}
				f.action(id, "status", "awaiting_receipt")
				f.action(id, "resume", "awaiting_receipt")
				if !reflect.DeepEqual(parts, []string{"1", "2"}) {
					t.Fatalf("uncertain part was replayed: %v", parts)
				}
			} else {
				if first.Progress.Stage != "uploading" || !reflect.DeepEqual(parts, []string{"1"}) {
					t.Fatalf("did not pause between confirmed parts: %+v parts=%v", first.Progress, parts)
				}
				record, err := f.app.currentAccount()
				if err != nil {
					t.Fatal(err)
				}
				record.RefreshToken, record.RefreshExpiresAt = "rft.fixture-token", f.app.nowUTC().Add(time.Hour)
				if err := f.app.writeAccount(record); err != nil {
					t.Fatal(err)
				}
				_, err = f.app.AuthorizeAccount(t.Context(), "owner/session", AccountAuthorizationInput{Action: "refresh", Revision: record.Revision})
				if err != nil {
					t.Fatal(err)
				}
				wantToken = "act.fixture-renewed-token"
				f.restart(transport)
				accepted := f.action(id, "resume", "accepted")
				if accepted.Progress == nil || accepted.Progress.Stage != "accepted" || !reflect.DeepEqual(parts, []string{"1", "2", "3"}) {
					t.Fatalf("resume did not preserve confirmation: accepted=%+v parts=%v", accepted, parts)
				}
			}
		})
	}
}

func TestDouyinPublicationStopsBeforeCreateWhenCancelRacesUploadReceipt(t *testing.T) {
	uploading, release := make(chan struct{}), make(chan struct{})
	var createCalls atomic.Int32
	f := newDouyinPublicationFixture(t, fixtureVideo(128), roundTrip(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/douyin/v1/video/upload_video/":
			close(uploading)
			select {
			case <-release:
				return jsonResponse(`{"data":{"error_code":0,"video":{"video_id":"upload-token"}},"extra":{"error_code":0}}`), nil
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		case "/api/douyin/v1/video/create_video/":
			createCalls.Add(1)
			return nil, errors.New("create should have been stopped")
		default:
			return nil, errors.New("unexpected endpoint")
		}
	}))
	id := f.save()
	completed := make(chan error, 1)
	go func() {
		_, err := f.publish.Action(t.Context(), f.actor, id, "submit")
		completed <- err
	}()
	select {
	case <-uploading:
	case err := <-completed:
		t.Fatalf("upload did not start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not reach the provider")
	}
	view, err := f.publish.Action(t.Context(), f.actor, id, "cancel")
	close(release)
	if completion := <-completed; completion != nil {
		t.Fatalf("late upload result: %v", completion)
	}
	if err != nil || view.State != "cancelled" || createCalls.Load() != 0 {
		t.Fatalf("cancel did not prevent creation: %+v err=%v create=%d", view, err, createCalls.Load())
	}
}

func TestDouyinPublicationConfirmedRefusalSettlesFailedWithoutRemoteID(t *testing.T) {
	createCalls := 0
	f := newDouyinPublicationFixture(t, fixtureVideo(128), roundTrip(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/douyin/v1/video/upload_video/":
			return jsonResponse(`{"data":{"error_code":0,"video":{"video_id":"upload-token"}},"extra":{"error_code":0}}`), nil
		case "/api/douyin/v1/video/create_video/":
			createCalls++
			return jsonResponse(`{"data":{"error_code":2190007},"extra":{"error_code":0}}`), nil
		default:
			t.Fatalf("unexpected endpoint %s", request.URL.Path)
			return nil, nil
		}
	}))
	id := f.save()
	failed := f.action(id, "submit", "failed")
	if failed.RemoteID != "" || createCalls != 1 || len(failed.AvailableActions) != 0 {
		t.Fatalf("refusal not terminally settled: %+v create=%d", failed, createCalls)
	}
}
