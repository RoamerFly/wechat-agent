package adapter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
)

func publicationFixture(t *testing.T, enabled bool, expiresAt time.Time) (*PublicApp, publicationSettings) {
	t.Helper()
	app := New(t.TempDir())
	settings := publicationSettings{
		SchemaVersion: 1, Enabled: enabled, ClientID: "client", AppSecret: "secret", AccessToken: "token",
		AccountID: "local-owner-42", ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	}
	writePublicationSettings(t, app.privateDirectory, settings)
	return app, settings
}

func writePublicationSettings(t *testing.T, directory string, settings publicationSettings) {
	t.Helper()
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, publicationSettingsName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func publicationRef(settings publicationSettings) apphost.ConnectionRef {
	return apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: publicationConnectionID, AccountID: publicationAccountID(settings.AccountID)}
}

func TestPublicationGrantIsolatedByExactInstallationAccountAndCredentials(t *testing.T) {
	app, settings := publicationFixture(t, true, time.Now().Add(time.Hour))
	ref := publicationRef(settings)
	grant, err := app.PublicationGrant(t.Context(), ref)
	if err != nil || !grant.Allowed || grant.Version == "" {
		t.Fatalf("grant = %#v, %v", grant, err)
	}
	for _, changed := range []apphost.ConnectionRef{
		{AppID: "other", InstallationID: installationID, ConnectionID: publicationConnectionID, AccountID: ref.AccountID},
		{AppID: ID, InstallationID: "other-install", ConnectionID: publicationConnectionID, AccountID: ref.AccountID},
		{AppID: ID, InstallationID: installationID, ConnectionID: connectionID, AccountID: ref.AccountID},
		{AppID: ID, InstallationID: installationID, ConnectionID: publicationConnectionID, AccountID: "authorized-other"},
	} {
		denied, err := app.PublicationGrant(t.Context(), changed)
		if err != nil || denied.Allowed {
			t.Fatalf("foreign reference %#v grant = %#v, %v", changed, denied, err)
		}
	}
	// A credential edit invalidates an already-issued host grant version.
	settings.AccessToken = "changed-token"
	writePublicationSettings(t, app.privateDirectory, settings)
	changed, err := app.PublicationGrant(t.Context(), ref)
	if err != nil || !changed.Allowed || changed.Version == grant.Version {
		t.Fatalf("credential change grant = %#v, %v; initial = %#v", changed, err, grant)
	}
}

func TestPublicationDisabledOrExpiredKeepsLocalCredentialsForReconciliation(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		enabled bool
		expires time.Time
	}{
		{name: "disabled", enabled: false, expires: time.Now().Add(time.Hour)},
		{name: "expired", enabled: true, expires: time.Now().Add(-time.Hour)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			app, settings := publicationFixture(t, scenario.enabled, scenario.expires)
			loaded, grant, err := app.publicationAccess(publicationRef(settings))
			if err != nil || grant.Allowed || loaded.AccessToken != settings.AccessToken || loaded.PartBytes != DefaultPublicationPartBytes {
				t.Fatalf("access = %#v, %#v, %v", loaded, grant, err)
			}
			status, err := app.Inspect(t.Context(), publicationConnectionID)
			if err != nil || status.Worker != "disabled" || len(status.SupportedActions) != 0 {
				t.Fatalf("status = %#v, %v", status, err)
			}
		})
	}
}

func TestPublicationRevocationStopsNewGrantButRetainsLocalReconciliationAccess(t *testing.T) {
	app, settings := publicationFixture(t, true, time.Now().Add(time.Hour))
	ref := publicationRef(settings)
	before, err := app.PublicationGrant(t.Context(), ref)
	if err != nil || !before.Allowed {
		t.Fatalf("initial grant = %#v, %v", before, err)
	}
	settings.Enabled = false
	writePublicationSettings(t, app.privateDirectory, settings)
	after, err := app.PublicationGrant(t.Context(), ref)
	if err != nil || after.Allowed || after.Version == before.Version {
		t.Fatalf("revoked grant = %#v, %v; initial = %#v", after, err, before)
	}
	loaded, grant, err := app.publicationAccess(ref)
	if err != nil || grant.Allowed || loaded.AccessToken != settings.AccessToken {
		t.Fatalf("revoked reconciliation access = %#v, %#v, %v", loaded, grant, err)
	}
}

func TestPublicationSettingsPartBytesAreExplicitlyBounded(t *testing.T) {
	app, settings := publicationFixture(t, true, time.Now().Add(time.Hour))
	settings.PartBytes = minPublicationPartBytes - 1
	writePublicationSettings(t, app.privateDirectory, settings)
	if _, _, err := app.publicationAccess(publicationRef(settings)); !errors.Is(err, ErrOpenUploadInvalid) {
		t.Fatalf("small part size error = %v, want explicit upload parameter error", err)
	}
	settings.PartBytes = maxPublicationPartBytes + 1
	writePublicationSettings(t, app.privateDirectory, settings)
	if _, _, err := app.publicationAccess(publicationRef(settings)); !errors.Is(err, ErrOpenUploadInvalid) {
		t.Fatalf("large part size error = %v, want explicit upload parameter error", err)
	}
}

func TestPublicationConnectionIsOmittedUntilOwnerConfiguresIt(t *testing.T) {
	app := New(t.TempDir())
	connections, err := app.Connections(t.Context())
	if err != nil || len(connections) != 1 || connections[0].ID != connectionID {
		t.Fatalf("connections = %#v, %v", connections, err)
	}
}
