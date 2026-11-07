package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
)

const enrollmentOpenID = "synthetic-open-id-1"

func enrollmentInput(revision string, creds OpenCredentials) PublicationAuthorizationInput {
	enabled := true
	return PublicationAuthorizationInput{
		Action: "verify", Revision: revision, ClientID: creds.ClientID, AppSecret: creds.AppSecret, AccessToken: creds.AccessToken,
		ExpiresAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339), Enabled: &enabled,
	}
}

func enrollmentCredentials(settings publicationSettings) OpenCredentials {
	return OpenCredentials{ClientID: settings.ClientID, AppSecret: settings.AppSecret, AccessToken: settings.AccessToken}
}

func TestPublicationEnrollmentVerifiesAccountAndRejectsForeignRotation(t *testing.T) {
	app := New(t.TempDir())
	creds := OpenCredentials{ClientID: "synthetic-app-id", AppSecret: "synthetic-app-secret", AccessToken: "synthetic-access-token"}
	calls := 0
	app.verifyPublicationAccount = func(_ context.Context, got OpenCredentials) (string, error) {
		calls++
		switch got.AccessToken {
		case "wrong-account-token":
			return "other-open-id", nil
		case "broken-access-token":
			return "", errors.New("private provider detail")
		default:
			return enrollmentOpenID, nil
		}
	}
	initial, err := app.PublicationAuthorization()
	if err != nil || initial.State != "unconfigured" || calls != 0 {
		t.Fatalf("unconfigured status: %+v %v", initial, err)
	}
	current, err := app.SavePublicationAuthorization(t.Context(), enrollmentInput(initial.Revision, creds))
	if err != nil || !current.Verified || !current.Enabled || current.State != "ready" || calls != 1 {
		t.Fatalf("verified status: %+v %v", current, err)
	}
	raw, _ := json.Marshal(current)
	for _, secret := range []string{creds.ClientID, creds.AppSecret, creds.AccessToken, enrollmentOpenID} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private authority data escaped the view")
		}
	}
	old, err := app.loadPublicationSettings()
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision == old.version {
		t.Fatal("the UI exposes the private credential digest")
	}
	for _, bad := range []struct {
		token string
		want  error
	}{{"wrong-account-token", ErrPublicationAuthorizationIdentity}, {"broken-access-token", ErrPublicationAuthorizationVerify}} {
		replacement := creds
		replacement.AccessToken = bad.token
		if _, err := app.SavePublicationAuthorization(t.Context(), enrollmentInput(current.Revision, replacement)); !errors.Is(err, bad.want) {
			t.Fatalf("reject unsafe replacement: %v", err)
		}
		after, _ := app.loadPublicationSettings()
		if after.version != old.version {
			t.Fatal("failed verification changed the stored grant")
		}
	}
	creds.AccessToken = "rotated-access-token"
	rotated, err := app.SavePublicationAuthorization(t.Context(), enrollmentInput(current.Revision, creds))
	after, readErr := app.loadPublicationSettings()
	if err != nil || readErr != nil || rotated.Revision == current.Revision || after.AccountID != old.AccountID ||
		after.version == old.version || after.AccessToken != creds.AccessToken {
		t.Fatalf("same-account rotation failed: %v, %v", err, readErr)
	}
	if _, err := app.SavePublicationAuthorization(t.Context(), enrollmentInput(current.Revision, creds)); !errors.Is(err, ErrPublicationAuthorizationConflict) || calls != 4 {
		t.Fatalf("stale version made a network call: %d %v", calls, err)
	}
}

func TestPublicationEnrollmentDoesNotOverwriteConcurrentDisable(t *testing.T) {
	app, settings := publicationFixture(t, true, time.Now().Add(time.Hour))
	initial, _ := app.PublicationAuthorization()
	app.verifyPublicationAccount = func(ctx context.Context, _ OpenCredentials) (string, error) {
		if _, err := app.SavePublicationAuthorization(ctx, PublicationAuthorizationInput{Action: "disable", Revision: initial.Revision}); err != nil {
			t.Fatal(err)
		}
		return enrollmentOpenID, nil
	}
	_, err := app.SavePublicationAuthorization(t.Context(), enrollmentInput(initial.Revision, enrollmentCredentials(settings)))
	view, _ := app.PublicationAuthorization()
	if !errors.Is(err, ErrPublicationAuthorizationConflict) || view.Enabled || view.Verified {
		t.Fatalf("stale verify resurrected disabled grant: %+v %v", view, err)
	}
}

func TestPublicationReconcileWithVerifiedRotationUsesOriginalResource(t *testing.T) {
	fixture := newBiliPublicationFixture(t)
	legacySettings := fixture.settings
	legacyJob := fixture.save()
	fixture.action(legacyJob, "submit", "accepted")
	verify := func(_ context.Context, _ OpenCredentials) (string, error) { return enrollmentOpenID, nil }
	fixture.app.verifyPublicationAccount = verify
	view, _ := fixture.app.PublicationAuthorization()
	replacement := enrollmentCredentials(legacySettings)
	replacement.AccessToken = "not-the-original-token"
	if _, err := fixture.app.SavePublicationAuthorization(t.Context(), enrollmentInput(view.Revision, replacement)); !errors.Is(err, ErrPublicationAuthorizationLegacy) {
		t.Fatalf("unchecked legacy identity was rebound: %v", err)
	}
	view, err := fixture.app.SavePublicationAuthorization(t.Context(), enrollmentInput(view.Revision, enrollmentCredentials(legacySettings)))
	if err != nil || !view.Verified {
		t.Fatalf("legacy credential verification: %+v %v", view, err)
	}
	current, _ := fixture.app.loadPublicationSettings()
	fixture.settings = current.publicationSettings
	fixture.actor.RequestKey = "managed-task-fixture"
	managedJob := fixture.save()
	fixture.action(managedJob, "submit", "accepted")
	fixture.actor.RequestKey = "managed-upload-fixture"
	uploadJob := fixture.save()
	fixture.provider.failPart = true
	fixture.action(uploadJob, "submit", "awaiting_receipt")
	writesBefore := len(fixture.provider.parts)

	_, err = fixture.app.SavePublicationAuthorization(t.Context(), enrollmentInput(view.Revision, replacement))
	if err != nil {
		t.Fatal(err)
	}
	current, err = fixture.app.loadPublicationSettings()
	if err != nil || current.AccountID != legacySettings.AccountID {
		t.Fatalf("rotation lost existing task target: %v", err)
	}
	fixture.settings = current.publicationSettings
	fixture.restart()
	if _, err = fixture.publisher.Action(t.Context(), fixture.actor, uploadJob, "resume"); !errors.Is(err, apphost.ErrPublicationDenied) {
		t.Fatalf("rotation unexpectedly resumed an old write: %v", err)
	}
	fixture.provider.published = true
	fixture.action(legacyJob, "status", "published")
	fixture.action(managedJob, "status", "published")
	if fixture.provider.queries != 2 || fixture.provider.posts != 2 || len(fixture.provider.parts) != writesBefore {
		t.Fatalf("reconciliation performed unexpected activity: %+v", fixture.provider)
	}
}
