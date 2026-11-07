package station

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/hostfile"
)

type identitySwitchObservation struct {
	*syntheticObservationApp
	grants         int
	switchIdentity func()
}

func (p *identitySwitchObservation) ObservationGrant(ctx context.Context, in apphost.ObservationAccess) (apphost.ObservationGrant, error) {
	grant, err := p.syntheticObservationApp.ObservationGrant(ctx, in)
	p.grants++
	// Switch immediately after the provider's final grant result was formed.
	if p.grants == 2 {
		p.switchIdentity()
	}
	return grant, err
}

func TestAppObservationToolRejectsIdentityChangeAfterProviderCheck(t *testing.T) {
	base := t.TempDir()
	data, workspace := filepath.Join(base, "host"), filepath.Join(base, "workspace")
	provider := &identitySwitchObservation{syntheticObservationApp: newSyntheticObservationApp()}
	registry, err := apphost.New(apphost.Registration{Application: apphost.Application{ID: "offline-library", Name: "合成资料", InstallationID: "offline-library-test", Registration: "synthetic"}, Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLocalHomeHandlerWithApps(data, workspace, "synthetic-local-password", "", registry)
	if err != nil {
		t.Fatal(err)
	}
	f := newLocalAppObservationFixtureWithHandler(t, handler, data, workspace)
	initializeDouyinWeb(t, f)
	store, in, err := handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	text := "读取获准正文"
	in.TraceID, in.Text, in.OriginalText = "identity-switch", text, &text
	in.Source = &companion.SourceRef{MessageID: in.TraceID}
	in.MessageTime = time.Now().UTC().Format(time.RFC3339Nano)
	if err := store.Observe(in, localConversationConfig(""), time.Now()); err != nil {
		t.Fatal(err)
	}
	run, err := store.OpenLocalToolRun(t.Context(), in, true)
	if err != nil {
		t.Fatal(err)
	}
	tools := handler.chat.tools(store, in)
	tools.run = run
	provider.switchIdentity = func() {
		record, err := handler.home.identity.Read()
		if err != nil {
			t.Error(err)
			return
		}
		record.FishID = "fish-" + strings.Repeat("b", 48)
		raw, _ := json.Marshal(record)
		if err := hostfile.WritePrivate(filepath.Join(data, "fish-identity.json"), raw); err != nil {
			t.Error(err)
		}
	}
	result, err := tools.callAppObservation(context.Background(), json.RawMessage(`{"action":"read","app_id":"offline-library","connection_id":"offline-connection","content_id":"night-sea","field":"body"}`))
	if !errors.Is(err, apphost.ErrObservationForbidden) || result != "" || len(tools.appReads) != 0 {
		t.Fatalf("another identity received old evidence: %q %v reads=%d", result, err, len(tools.appReads))
	}
}
