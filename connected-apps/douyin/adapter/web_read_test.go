package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"wechat-bot/internal/apphost"
)

type webReadStub struct {
	connection apphost.Connection
	present    bool
	connErr    error
	targets    []apphost.ObservationTarget
	targetErr  error
	grant      apphost.ObservationGrant
	grantErr   error
	grantFunc  func(apphost.ObservationAccess) (apphost.ObservationGrant, error)
	page       apphost.ObservationPage
	readErr    error
	request    apphost.ObservationRequest
	targetsHit int
	grantHit   int
	readHit    int
}

func (s *webReadStub) Connection(context.Context) (apphost.Connection, bool, error) {
	return s.connection, s.present, s.connErr
}
func (s *webReadStub) Targets(context.Context) ([]apphost.ObservationTarget, error) {
	s.targetsHit++
	return s.targets, s.targetErr
}
func (s *webReadStub) Grant(_ context.Context, access apphost.ObservationAccess) (apphost.ObservationGrant, error) {
	s.grantHit++
	if s.grantFunc != nil {
		return s.grantFunc(access)
	}
	return s.grant, s.grantErr
}
func (s *webReadStub) Read(_ context.Context, request apphost.ObservationRequest) (apphost.ObservationPage, error) {
	s.readHit++
	s.request = request
	return s.page, s.readErr
}

func TestWebReadConnectionIsOptionalAndSeparatelyDescribed(t *testing.T) {
	app := New(t.TempDir())
	connections, err := app.Connections(t.Context())
	if err != nil || len(connections) != 3 {
		t.Fatalf("connections without callback = %#v, %v", connections, err)
	}

	stub := &webReadStub{connection: apphost.Connection{ID: WebReadConnectionID, AccountID: "saved-web-profile", Label: "抖音主页文字", Configuration: "ready"}, present: true}
	app.SetWebReadConnection(stub)
	connections, err = app.Connections(t.Context())
	if err != nil || len(connections) != 4 || connections[3] != stub.connection {
		t.Fatalf("connections with callback = %#v, %v", connections, err)
	}
	status, err := app.Inspect(t.Context(), WebReadConnectionID)
	if err != nil || status.Login != "saved网页登录资料" || status.Runtime != "local" || status.Worker != "ready" || status.CapabilityCheck != "ready" || len(status.SupportedActions) != 1 || status.SupportedActions[0] != "content.page-text" {
		t.Fatalf("web read status = %#v, %v", status, err)
	}
}

func TestWithWebReadConnectionKeepsStartupInstancesIsolated(t *testing.T) {
	original := New(t.TempDir())
	first := &webReadStub{connection: apphost.Connection{ID: WebReadConnectionID, AccountID: "first-profile", Configuration: "ready"}, present: true}
	second := &webReadStub{connection: apphost.Connection{ID: WebReadConnectionID, AccountID: "second-profile", Configuration: "ready"}, present: true}
	firstApp := original.WithWebReadConnection(first)
	secondApp := original.WithWebReadConnection(second)
	if firstApp == original || secondApp == original || firstApp == secondApp {
		t.Fatal("web-read startup instances must be independent")
	}
	if connections, err := original.Connections(t.Context()); err != nil || len(connections) != 3 {
		t.Fatalf("original changed by WithWebReadConnection: %#v, %v", connections, err)
	}
	for _, tc := range []struct {
		name string
		app  *SearchApp
		want string
	}{
		{name: "first", app: firstApp, want: "first-profile"},
		{name: "second", app: secondApp, want: "second-profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connections, err := tc.app.Connections(t.Context())
			if err != nil || len(connections) != 4 || connections[3].AccountID != tc.want {
				t.Fatalf("connection = %#v, %v", connections, err)
			}
			status, err := tc.app.Inspect(t.Context(), WebReadConnectionID)
			if err != nil || status.AccountID != tc.want {
				t.Fatalf("status = %#v, %v", status, err)
			}
		})
	}
}

func TestWebReadOnlyDelegatesExactReferenceAndAuthorizedVideoPageText(t *testing.T) {
	app := New(t.TempDir())
	ref := apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: WebReadConnectionID, AccountID: "saved-web-profile"}
	stub := &webReadStub{
		connection: apphost.Connection{ID: WebReadConnectionID, AccountID: ref.AccountID, Configuration: "ready"}, present: true,
		targets: []apphost.ObservationTarget{{ConnectionRef: ref, ContentID: WebReadContentID, Fields: []apphost.ObservationField{apphost.ObservationPageText}}},
		page:    apphost.ObservationPage{Coverage: "host page text", Complete: true},
	}
	videoID := WebVideoContentPrefix + "hostSigned_1-abc"
	stub.grantFunc = func(access apphost.ObservationAccess) (apphost.ObservationGrant, error) {
		if access.ContentID == WebReadContentID || access.ContentID == videoID {
			return apphost.ObservationGrant{Allowed: true, Version: "host-web-grant-v1", Fresh: true}, nil
		}
		return apphost.ObservationGrant{Allowed: false}, nil
	}
	app.SetWebReadConnection(stub)
	targets, err := app.ObservationTargets(t.Context(), WebReadConnectionID)
	if err != nil || len(targets) != 1 || targets[0].ConnectionRef != ref || targets[0].ContentID != WebReadContentID || len(targets[0].Fields) != 1 || targets[0].Fields[0] != apphost.ObservationPageText {
		t.Fatalf("targets = %#v, %v", targets, err)
	}
	access := apphost.ObservationAccess{ConnectionRef: ref, ContentID: WebReadContentID, Field: apphost.ObservationPageText}
	grant, err := app.ObservationGrant(t.Context(), access)
	if err != nil || !grant.Allowed || grant.Version != "host-web-grant-v1" || stub.grantHit != 1 {
		t.Fatalf("grant = %#v, %v, hits=%d", grant, err, stub.grantHit)
	}
	page, err := app.ReadObservation(t.Context(), apphost.ObservationRequest{ObservationAccess: access, Cursor: "host-owned", Limit: 99})
	if err != nil || page.Coverage != "host page text" || stub.readHit != 1 {
		t.Fatalf("page = %#v, %v, hits=%d", page, err, stub.readHit)
	}
	videoAccess := apphost.ObservationAccess{ConnectionRef: ref, ContentID: videoID, Field: apphost.ObservationPageText}
	grant, err = app.ObservationGrant(t.Context(), videoAccess)
	if err != nil || !grant.Allowed || grant.Version != "host-web-grant-v1" || stub.grantHit != 2 {
		t.Fatalf("video grant = %#v, %v, hits=%d", grant, err, stub.grantHit)
	}
	if _, err := app.ReadObservation(t.Context(), apphost.ObservationRequest{ObservationAccess: videoAccess}); err != nil || stub.readHit != 2 {
		t.Fatalf("video read = %v, hits=%d", err, stub.readHit)
	}
	unknown := apphost.ObservationAccess{ConnectionRef: ref, ContentID: WebVideoContentPrefix + "notHostGranted", Field: apphost.ObservationPageText}
	grant, err = app.ObservationGrant(t.Context(), unknown)
	if err != nil || grant.Allowed || stub.grantHit != 3 {
		t.Fatalf("unknown prefixed ID bypassed host grant: %#v, %v, hits=%d", grant, err, stub.grantHit)
	}

	for _, denied := range []apphost.ObservationAccess{
		{ConnectionRef: apphost.ConnectionRef{AppID: "other", InstallationID: installationID, ConnectionID: WebReadConnectionID, AccountID: ref.AccountID}, ContentID: WebReadContentID, Field: apphost.ObservationPageText},
		{ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: "other", ConnectionID: WebReadConnectionID, AccountID: ref.AccountID}, ContentID: WebReadContentID, Field: apphost.ObservationPageText},
		{ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: WebReadConnectionID, AccountID: "other"}, ContentID: WebReadContentID, Field: apphost.ObservationPageText},
		{ConnectionRef: ref, ContentID: "other", Field: apphost.ObservationPageText},
		{ConnectionRef: ref, ContentID: WebVideoContentPrefix + "not=base64url", Field: apphost.ObservationPageText},
		{ConnectionRef: ref, ContentID: WebReadContentID, Field: apphost.ObservationTitle},
	} {
		grant, err := app.ObservationGrant(t.Context(), denied)
		if err != nil || grant.Allowed {
			t.Fatalf("grant unexpectedly expanded access %#v: %#v, %v", denied, grant, err)
		}
		if _, err := app.ReadObservation(t.Context(), apphost.ObservationRequest{ObservationAccess: denied}); !errors.Is(err, apphost.ErrObservationForbidden) {
			t.Fatalf("read unexpectedly expanded access %#v: %v", denied, err)
		}
	}
	if stub.grantHit != 3 || stub.readHit != 2 {
		t.Fatalf("denied requests reached callback: grant=%d read=%d", stub.grantHit, stub.readHit)
	}

	stub.targets = []apphost.ObservationTarget{{ConnectionRef: ref, ContentID: WebReadContentID, Fields: []apphost.ObservationField{apphost.ObservationTitle}}}
	if _, err := app.ObservationTargets(t.Context(), WebReadConnectionID); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("invalid callback target accepted: %v", err)
	}
	stub.targets = []apphost.ObservationTarget{{ConnectionRef: ref, ContentID: videoID, Fields: []apphost.ObservationField{apphost.ObservationPageText}}}
	if _, err := app.ObservationTargets(t.Context(), WebReadConnectionID); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("video callback target accepted instead of home-only discovery: %v", err)
	}
}

func TestIsWebReadContentIDOnlyChecksOpaqueVideoSyntax(t *testing.T) {
	if !IsWebReadContentID(WebReadContentID) || !IsWebReadContentID(WebVideoContentPrefix+"AZaz09-_") {
		t.Fatal("valid web-read content ID rejected")
	}
	for _, contentID := range []string{
		"", "video", WebVideoContentPrefix, WebVideoContentPrefix + "bad=padding", WebVideoContentPrefix + "contains space", WebVideoContentPrefix + strings.Repeat("a", 221),
	} {
		if IsWebReadContentID(contentID) {
			t.Fatalf("invalid web-read content ID accepted: %q", contentID)
		}
	}
}

func TestWebVideoPlaybackDelegatesExactHostGrantAndRequest(t *testing.T) {
	ref := apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: WebReadConnectionID, AccountID: "saved-web-profile"}
	videoID := WebVideoContentPrefix + "hostVideo_1-abc"
	access := apphost.ObservationAccess{ConnectionRef: ref, ContentID: videoID, Field: apphost.ObservationPlayback}
	stub := &webReadStub{
		connection: apphost.Connection{ID: WebReadConnectionID, AccountID: ref.AccountID, Configuration: "ready"}, present: true,
		grant: apphost.ObservationGrant{Allowed: true, Version: "explicit-web-playback-grant", Fresh: true},
		page: apphost.ObservationPage{Coverage: "host verified playback", Complete: true,
			Playback:      []apphost.PlaybackInterval{{StartSeconds: 0, EndSeconds: 1.25}},
			PlaybackProof: &apphost.PlaybackProof{RequestedSeconds: 17, MediaDurationSeconds: 20, Ready: true, Paused: true}},
	}
	app := New(t.TempDir()).WithWebReadConnection(stub)
	grant, err := app.ObservationGrant(t.Context(), access)
	if err != nil || grant != stub.grant || stub.grantHit != 1 {
		t.Fatalf("playback grant = %#v, %v, hits=%d", grant, err, stub.grantHit)
	}
	request := apphost.ObservationRequest{ObservationAccess: access, Cursor: "host-checkpoint", Limit: 17}
	page, err := app.ReadObservation(t.Context(), request)
	if err != nil || page.Coverage != stub.page.Coverage || len(page.Playback) != 1 || page.Playback[0] != stub.page.Playback[0] || page.PlaybackProof != stub.page.PlaybackProof || stub.request != request || stub.readHit != 1 {
		t.Fatalf("playback delegation = %#v, %v, request=%#v, hits=%d", page, err, stub.request, stub.readHit)
	}

	// Valid syntax never supplies authorization. The current host may deny a
	// formerly accepted target or independently disable playback at any time.
	stub.grant = apphost.ObservationGrant{Allowed: false}
	stub.page = apphost.ObservationPage{}
	stub.readErr = apphost.ErrObservationForbidden
	grant, err = app.ObservationGrant(t.Context(), access)
	if err != nil || grant.Allowed || stub.grantHit != 2 {
		t.Fatalf("host playback denial lost: %#v, %v", grant, err)
	}
	if page, err := app.ReadObservation(t.Context(), request); !errors.Is(err, apphost.ErrObservationForbidden) || len(page.Playback) != 0 || page.PlaybackProof != nil {
		t.Fatalf("denied playback result returned: %#v, %v", page, err)
	}
	grantHits, readHits := stub.grantHit, stub.readHit
	for _, denied := range []apphost.ObservationAccess{
		{ConnectionRef: ref, ContentID: WebReadContentID, Field: apphost.ObservationPlayback},
		{ConnectionRef: ref, ContentID: videoID, Field: apphost.ObservationOwnVideo},
		{ConnectionRef: ref, ContentID: videoID, Field: apphost.ObservationTitle},
		{ConnectionRef: ref, ContentID: WebVideoContentPrefix + "bad=padding", Field: apphost.ObservationPlayback},
		{ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: WebReadConnectionID, AccountID: "other-profile"}, ContentID: videoID, Field: apphost.ObservationPlayback},
		{ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: "other-installation", ConnectionID: WebReadConnectionID, AccountID: ref.AccountID}, ContentID: videoID, Field: apphost.ObservationPlayback},
		{ConnectionRef: apphost.ConnectionRef{AppID: "other-app", InstallationID: installationID, ConnectionID: WebReadConnectionID, AccountID: ref.AccountID}, ContentID: videoID, Field: apphost.ObservationPlayback},
	} {
		if grant, err := app.ObservationGrant(t.Context(), denied); err != nil || grant.Allowed {
			t.Fatalf("invalid playback grant accepted: %#v, %#v, %v", denied, grant, err)
		}
		if _, err := app.ReadObservation(t.Context(), apphost.ObservationRequest{ObservationAccess: denied}); !errors.Is(err, apphost.ErrObservationForbidden) {
			t.Fatalf("invalid playback request reached host: %#v, %v", denied, err)
		}
	}
	if stub.grantHit != grantHits || stub.readHit != readHits {
		t.Fatalf("invalid playback reached callback: grants %d/%d, reads %d/%d", stub.grantHit, grantHits, stub.readHit, readHits)
	}
}

func TestWebVideoPlaybackCallbacksRemainIsolatedAndDiscoveryStaysHomeText(t *testing.T) {
	ref := apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: WebReadConnectionID, AccountID: "same-synthetic-profile"}
	video := apphost.ObservationAccess{ConnectionRef: ref, ContentID: WebVideoContentPrefix + "sameSyntaxDifferentOwnerGrant", Field: apphost.ObservationPlayback}
	first := &webReadStub{connection: apphost.Connection{ID: ref.ConnectionID, AccountID: ref.AccountID, Configuration: "ready"}, present: true,
		grant:   apphost.ObservationGrant{Allowed: true, Version: "first-owner"},
		targets: []apphost.ObservationTarget{{ConnectionRef: ref, ContentID: WebReadContentID, Fields: []apphost.ObservationField{apphost.ObservationPageText}}}}
	second := &webReadStub{connection: first.connection, present: true, grant: apphost.ObservationGrant{Allowed: false}}
	original := New(t.TempDir())
	firstApp, secondApp := original.WithWebReadConnection(first), original.WithWebReadConnection(second)
	if grant, err := firstApp.ObservationGrant(t.Context(), video); err != nil || !grant.Allowed || grant.Version != "first-owner" {
		t.Fatalf("first callback = %#v, %v", grant, err)
	}
	if grant, err := secondApp.ObservationGrant(t.Context(), video); err != nil || grant.Allowed {
		t.Fatalf("second callback borrowed grant = %#v, %v", grant, err)
	}
	if first.grantHit != 1 || second.grantHit != 1 {
		t.Fatalf("callback identity mixed: first=%d second=%d", first.grantHit, second.grantHit)
	}
	targets, err := firstApp.ObservationTargets(t.Context(), WebReadConnectionID)
	if err != nil || len(targets) != 1 || targets[0].ContentID != WebReadContentID || len(targets[0].Fields) != 1 || targets[0].Fields[0] != apphost.ObservationPageText {
		t.Fatalf("playback changed initial text discovery: %#v, %v", targets, err)
	}
	first.targets[0].Fields = []apphost.ObservationField{apphost.ObservationPageText, apphost.ObservationPlayback}
	if _, err := firstApp.ObservationTargets(t.Context(), WebReadConnectionID); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("homepage playback incorrectly discoverable: %v", err)
	}
}

func TestWebVideoAudioKeepsHostGrantProgressAndRecognitionState(t *testing.T) {
	ref := apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: WebReadConnectionID, AccountID: "saved-web-profile"}
	access := apphost.ObservationAccess{ConnectionRef: ref, ContentID: WebVideoContentPrefix + "hostAudio_1-abc", Field: apphost.ObservationAudio}
	stub := &webReadStub{connection: apphost.Connection{ID: ref.ConnectionID, AccountID: ref.AccountID, Configuration: "ready"}, present: true,
		grant: apphost.ObservationGrant{Allowed: true, Version: "explicit-web-audio-grant"}}
	app := New(t.TempDir()).WithWebReadConnection(stub)
	grant, err := app.ObservationGrant(t.Context(), access)
	if err != nil || grant != stub.grant || stub.grantHit != 1 {
		t.Fatalf("audio grant = %#v, %v", grant, err)
	}
	for _, state := range []string{"transcribed", "no_recognizable_speech", "no_audio_in_range", "no_audio_track"} {
		t.Run(state, func(t *testing.T) {
			stub.page = apphost.ObservationPage{Coverage: "host verified audio", Complete: state == "no_audio_track", NextCursor: "host-next-window",
				AudioProof: &apphost.AudioProof{State: state, MediaSHA256: strings.Repeat("1", 64), RequestedSeconds: 240, StartSeconds: 240, WindowEndSeconds: 480, MediaDurationSeconds: 600}}
			if state == "no_audio_track" {
				stub.page.NextCursor = ""
			} else if state == "transcribed" || state == "no_recognizable_speech" {
				stub.page.AudioProof.DecodedSeconds = 240
			}
			if state == "transcribed" {
				stub.page.Chunks = []apphost.ObservationChunk{{Position: 240, Text: "合成音轨第二窗口。", SourceID: "host-audio-source"}}
			}
			request := apphost.ObservationRequest{ObservationAccess: access, Cursor: "host-audio-window", Limit: 240}
			page, err := app.ReadObservation(t.Context(), request)
			if err != nil || page.AudioProof != stub.page.AudioProof || page.NextCursor != stub.page.NextCursor || page.Complete != stub.page.Complete || len(page.Chunks) != len(stub.page.Chunks) || stub.request != request || len(page.Playback) != 0 || page.PlaybackProof != nil {
				t.Fatalf("audio result or progress changed: %#v, %v, request=%#v", page, err, stub.request)
			}
		})
	}
	// ASR/service errors stay errors, never a fabricated no-speech result.
	for _, failure := range []error{context.DeadlineExceeded, apphost.ErrObservationAudioUnavailable, apphost.ErrObservationAudioDecode, apphost.ErrObservationAudioTranscription, apphost.ErrObservationAudioSourceChanged} {
		stub.page, stub.readErr = apphost.ObservationPage{}, failure
		if page, err := app.ReadObservation(t.Context(), apphost.ObservationRequest{ObservationAccess: access, Limit: 60}); !errors.Is(err, failure) || page.AudioProof != nil {
			t.Fatalf("audio failure %v became recognition result: %#v, %v", failure, page, err)
		}
	}
	stub.grant = apphost.ObservationGrant{Allowed: false}
	if grant, err := app.ObservationGrant(t.Context(), access); err != nil || grant.Allowed {
		t.Fatalf("audio host denial lost: %#v, %v", grant, err)
	}
	grantHits, readHits := stub.grantHit, stub.readHit
	for _, denied := range []apphost.ObservationAccess{
		{ConnectionRef: ref, ContentID: WebReadContentID, Field: apphost.ObservationAudio},
		{ConnectionRef: ref, ContentID: WebVideoContentPrefix + "bad=padding", Field: apphost.ObservationAudio},
		{ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: WebReadConnectionID, AccountID: "another-owner-profile"}, ContentID: access.ContentID, Field: apphost.ObservationAudio},
	} {
		if grant, err := app.ObservationGrant(t.Context(), denied); err != nil || grant.Allowed {
			t.Fatalf("invalid audio grant accepted: %#v, %#v, %v", denied, grant, err)
		}
		if _, err := app.ReadObservation(t.Context(), apphost.ObservationRequest{ObservationAccess: denied}); !errors.Is(err, apphost.ErrObservationForbidden) {
			t.Fatalf("invalid audio access reached callback: %#v, %v", denied, err)
		}
	}
	if stub.grantHit != grantHits || stub.readHit != readHits {
		t.Fatalf("invalid audio called host: grants %d/%d reads %d/%d", stub.grantHit, grantHits, stub.readHit, readHits)
	}
}

func TestWebReadDoesNotInterceptOAuthAndPassesCallbackFailures(t *testing.T) {
	app := New(t.TempDir())
	ref := apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: WebReadConnectionID, AccountID: "saved-web-profile"}
	stub := &webReadStub{connection: apphost.Connection{ID: WebReadConnectionID, AccountID: ref.AccountID, Configuration: "ready"}, present: true, grantErr: context.Canceled, readErr: context.DeadlineExceeded}
	app.SetWebReadConnection(stub)
	access := apphost.ObservationAccess{ConnectionRef: ref, ContentID: WebReadContentID, Field: apphost.ObservationPageText}
	if _, err := app.ObservationGrant(t.Context(), access); !errors.Is(err, context.Canceled) {
		t.Fatalf("grant error = %v", err)
	}
	if _, err := app.ReadObservation(t.Context(), apphost.ObservationRequest{ObservationAccess: access}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read error = %v", err)
	}
	grantHits, readHits := stub.grantHit, stub.readHit
	if _, err := app.ObservationGrant(t.Context(), apphost.ObservationAccess{ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: accountConnectionID}, ContentID: "account", Field: apphost.ObservationAccountWorks}); err != nil && !errors.Is(err, apphost.ErrObservationCredentialExpired) && !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("oauth grant unexpectedly failed: %v", err)
	}
	if stub.grantHit != grantHits || stub.readHit != readHits {
		t.Fatalf("oauth path called web callback: grant=%d read=%d", stub.grantHit, stub.readHit)
	}
}
