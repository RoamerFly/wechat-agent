package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/pluginhost"
)

const fixturePublicationResource = "BV1aB2cD3e4F"
const fixturePublicationToken = "fixture-stage-private"

type fixturePublicationTransport struct {
	initializations, merges, posts, queries int
	parts                                   []int64
	failPart, failPost, published           bool
	onPart                                  func(int64)
	onPost, onView                          func()
	media                                   []byte
	t                                       *testing.T
}

func (f *fixturePublicationTransport) Initialize(context.Context, string) (string, error) {
	f.initializations++
	return fixturePublicationToken, nil
}
func (f *fixturePublicationTransport) UploadPart(_ context.Context, token string, part int64, data []byte) error {
	f.parts = append(f.parts, part)
	offset := int(part-1) * minPublicationPartBytes
	if token != fixturePublicationToken || !bytes.Equal(data, f.media[offset:min(offset+minPublicationPartBytes, len(f.media))]) {
		f.t.Fatal("wrong upload token or bytes")
	}
	if f.onPart != nil {
		f.onPart(part)
	}
	if f.failPart && part == 2 {
		return errors.New("simulated disconnect during part upload")
	}
	return nil
}
func (f *fixturePublicationTransport) Complete(context.Context, string) error {
	f.merges++
	return nil
}
func (f *fixturePublicationTransport) Post(_ context.Context, token string, metadata ArchiveMetadata) (string, error) {
	f.posts++
	if token != fixturePublicationToken || metadata.Title != "合成稿件" || metadata.CategoryID != 17 || metadata.Copyright != 1 ||
		!reflect.DeepEqual(metadata.Tags, []string{"合成"}) {
		f.t.Fatal("submission was not bound to the original selected metadata and token")
	}
	if f.onPost != nil {
		f.onPost()
	}
	if f.failPost {
		return "", errors.New("simulated loss of post response")
	}
	return fixturePublicationResource, nil
}
func (f *fixturePublicationTransport) View(_ context.Context, id string) (ArchiveStatus, error) {
	f.queries++
	if id != fixturePublicationResource {
		f.t.Fatal("query used an unrelated resource")
	}
	if f.onView != nil {
		f.onView()
	}
	return ArchiveStatus{Published: f.published}, nil
}

type biliPublicationFixture struct {
	t         *testing.T
	app       *PublicApp
	settings  publicationSettings
	provider  *fixturePublicationTransport
	store     *pluginhost.Store
	publisher *apphost.Publisher
	actor     pluginhost.Invocation
	draft     apphost.PublicationDraft
	storeDir  string
}

func newBiliPublicationFixture(t *testing.T) *biliPublicationFixture {
	t.Helper()
	app, settings := publicationFixture(t, true, time.Now().Add(time.Hour))
	settings.PartBytes = minPublicationPartBytes
	writePublicationSettings(t, app.privateDirectory, settings)
	media := bytes.Repeat([]byte{0x5a}, minPublicationPartBytes+19)
	copy(media, []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'})
	media[len(media)-1] = 0xa5
	hash := sha256.Sum256(media)
	f := &biliPublicationFixture{
		t: t, app: app, settings: settings, storeDir: t.TempDir(),
		provider: &fixturePublicationTransport{t: t, media: media},
		actor: pluginhost.Invocation{Account: "fish-fixture", Thread: "owner-fixture", Requester: "owner-fixture",
			Purpose: "synthetic publication", RequestKey: "request-fixture", Authorization: "home-fixture"},
		draft: apphost.PublicationDraft{Target: publicationRef(settings), Title: "合成稿件", Body: "合成简介",
			Artifacts: []apphost.PublicationArtifact{{ID: "artifact-fixture", SHA256: hex.EncodeToString(hash[:]), Name: "video.mp4", Bytes: int64(len(media))}},
			Options:   json.RawMessage(`{"tags":["合成"],"copyright":1,"category_id":17}`), Origin: json.RawMessage(`{"source":"fixture"}`)},
	}
	f.restart()
	t.Cleanup(func() { _ = f.store.Close() })
	return f
}

func (f *biliPublicationFixture) restart() {
	f.t.Helper()
	if f.store != nil {
		if err := f.store.Close(); err != nil {
			f.t.Fatal(err)
		}
		f.app = New(f.app.privateDirectory)
	}
	f.app.publicationClient = func(creds OpenCredentials) (publicationTransport, error) {
		if creds.AccessToken != f.settings.AccessToken || creds.ClientID != f.settings.ClientID {
			f.t.Fatal("used a different account's credentials")
		}
		return f.provider, nil
	}
	var err error
	f.store, err = pluginhost.OpenStore(f.storeDir, f.actor.Account)
	if err != nil {
		f.t.Fatal(err)
	}
	registry, err := apphost.New(f.app.Registration())
	if err != nil {
		f.t.Fatal(err)
	}
	f.publisher = apphost.NewPublisher(registry, f.store, nil, func(_ context.Context, ref apphost.PublicationArtifact) (io.ReadCloser, error) {
		if ref != f.draft.Artifacts[0] {
			return nil, errors.New("not the bound artifact")
		}
		return io.NopCloser(bytes.NewReader(f.provider.media)), nil
	})
}

func (f *biliPublicationFixture) save() string {
	f.t.Helper()
	view, err := f.publisher.Draft(f.t.Context(), f.actor, f.draft)
	if err != nil || view.State != "draft" {
		f.t.Fatalf("create draft: %+v %v", view, err)
	}
	return view.JobID
}

func (f *biliPublicationFixture) action(id, action, wantState string) apphost.PublicationView {
	f.t.Helper()
	view, err := f.publisher.Action(f.t.Context(), f.actor, id, action)
	if err != nil || view.State != wantState {
		f.t.Fatalf("%s: %+v %v, want %s", action, view, err, wantState)
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), fixturePublicationToken) || strings.Contains(string(raw), f.settings.AccountID) {
		f.t.Fatal("private platform metadata leaked through task view")
	}
	return view
}

func TestPublicationWorkflowResumesOnlyUnconfirmedPartAfterRestart(t *testing.T) {
	f := newBiliPublicationFixture(t)
	id := f.save()
	targets := f.publisher.Discover(t.Context())
	if !reflect.DeepEqual(targets, []apphost.ConnectionRef{f.draft.Target}) {
		t.Fatalf("anonymous connection exposed as a publication target: %+v", targets)
	}
	f.provider.failPart = true
	first := f.action(id, "submit", "awaiting_receipt")
	if first.Progress == nil || first.Progress.Stage != "uploading" || first.Progress.ConfirmedParts != 1 || first.Progress.TotalParts != 2 {
		t.Fatalf("part progress was not reported: %+v", first)
	}
	f.restart()
	f.action(id, "status", "awaiting_receipt")
	if !reflect.DeepEqual(f.provider.parts, []int64{1, 2}) || f.provider.posts != 0 {
		t.Fatalf("read-only status retried a write: %+v", f.provider)
	}
	f.provider.failPart = false
	accepted := f.action(id, "resume", "accepted")
	if accepted.RemoteID != fixturePublicationResource || !reflect.DeepEqual(f.provider.parts, []int64{1, 2, 2}) ||
		f.provider.initializations != 1 || f.provider.merges != 1 || f.provider.posts != 1 {
		t.Fatalf("resume did not continue original upload: %+v", f.provider)
	}
	f.restart()
	f.action(id, "status", "accepted")
	settings := f.settings
	settings.Enabled = false
	writePublicationSettings(t, f.app.privateDirectory, settings)
	f.provider.published = true
	f.action(id, "status", "published")
	if f.provider.posts != 1 || f.provider.queries != 2 {
		t.Fatalf("unexpected receipt queries or submissions: %+v", f.provider)
	}
}

func TestPublicationWorkflowDoesNotRepeatPostWithLostResponse(t *testing.T) {
	f := newBiliPublicationFixture(t)
	id := f.save()
	f.provider.failPost = true
	first := f.action(id, "submit", "awaiting_receipt")
	if first.Progress == nil || first.Progress.Stage != "posting" {
		t.Fatalf("missing durable posting claim: %+v", first)
	}
	f.restart()
	f.action(id, "status", "awaiting_receipt")
	f.action(id, "resume", "awaiting_receipt")
	stopped := f.action(id, "cancel", "awaiting_receipt")
	if !stopped.CancelRequested || f.provider.posts != 1 || f.provider.merges != 1 || f.provider.queries != 0 {
		t.Fatalf("uncertain post replayed or incorrectly cancelled: %+v, %+v", f.provider, stopped)
	}
}

func TestPublicationListUsesSafeDurableProgressAndActionsWithoutRemoteCalls(t *testing.T) {
	f := newBiliPublicationFixture(t)
	id := f.save()
	page, err := f.publisher.List(t.Context(), f.actor, ID, 0, 10)
	if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0].AvailableActions, []string{"cancel", "submit"}) {
		t.Fatalf("draft actions were not authorization scoped: %+v %v", page, err)
	}
	f.provider.failPart = true
	f.action(id, "submit", "awaiting_receipt")

	page, err = f.publisher.List(t.Context(), f.actor, ID, 0, 10)
	if err != nil || len(page.Items) != 1 || page.HasMore || page.Next < 1 || page.Limit != 10 {
		t.Fatalf("unexpected publication page: %+v %v", page, err)
	}
	item := page.Items[0]
	if item.Progress == nil || item.Progress.Stage != "uploading" || item.Progress.ConfirmedParts != 1 || item.Progress.TotalParts != 2 || item.UpdatedAt == "" {
		t.Fatalf("list did not expose durable safe progress and update time: %+v", item)
	}
	if !reflect.DeepEqual(item.AvailableActions, []string{"status", "cancel", "resume"}) {
		t.Fatalf("upload checkpoint actions: %#v", item.AvailableActions)
	}
	if f.provider.queries != 0 || f.provider.posts != 0 || f.provider.merges != 0 {
		t.Fatalf("list made remote publication calls: %+v", f.provider)
	}
	raw, _ := json.Marshal(item)
	if strings.Contains(string(raw), fixturePublicationToken) || strings.Contains(string(raw), f.settings.AccessToken) || strings.Contains(string(raw), "合成稿件") || strings.Contains(string(raw), "合成简介") {
		t.Fatalf("list leaked private task material: %s", raw)
	}

	settings := f.settings
	settings.Enabled = false
	writePublicationSettings(t, f.app.privateDirectory, settings)
	page, err = f.publisher.List(t.Context(), f.actor, ID, 0, 10)
	if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0].AvailableActions, []string{"status", "cancel"}) {
		t.Fatalf("revoked grant still offered resume: %+v %v", page, err)
	}

	// A post response can be uncertain; list must preserve the durable stage
	// but must not offer replay through resume.
	f = newBiliPublicationFixture(t)
	id = f.save()
	f.provider.failPost = true
	f.action(id, "submit", "awaiting_receipt")
	page, err = f.publisher.List(t.Context(), f.actor, ID, 0, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Progress == nil || page.Items[0].Progress.Stage != "posting" || !reflect.DeepEqual(page.Items[0].AvailableActions, []string{"status", "cancel"}) {
		t.Fatalf("posting uncertainty offered unsafe actions: %+v %v", page, err)
	}
	if f.provider.queries != 0 || f.provider.posts != 1 {
		t.Fatalf("list queried or reposted uncertain work: %+v", f.provider)
	}

	f = newBiliPublicationFixture(t)
	id = f.save()
	f.action(id, "cancel", "cancelled")
	page, err = f.publisher.List(t.Context(), f.actor, ID, 0, 10)
	if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0].AvailableActions, []string{}) {
		t.Fatalf("terminal task kept an actionable button: %+v %v", page, err)
	}
}

func TestPublicationWorkflowStopsBeforeNextPartWhenCancelledOrRevoked(t *testing.T) {
	for _, action := range []string{"cancel", "revoke"} {
		t.Run(action, func(t *testing.T) {
			f := newBiliPublicationFixture(t)
			id := f.save()
			f.provider.onPart = func(number int64) {
				if number != 1 {
					return
				}
				if action == "cancel" {
					f.action(id, "cancel", "cancelled")
				} else {
					settings := f.settings
					settings.Enabled = false
					writePublicationSettings(t, f.app.privateDirectory, settings)
				}
			}
			wantState := "awaiting_receipt"
			if action == "cancel" {
				wantState = "cancelled"
			}
			f.action(id, "submit", wantState)
			if !reflect.DeepEqual(f.provider.parts, []int64{1}) || f.provider.merges != 0 || f.provider.posts != 0 {
				t.Fatalf("outbound work continued after %s: %+v", action, f.provider)
			}
		})
	}
}
