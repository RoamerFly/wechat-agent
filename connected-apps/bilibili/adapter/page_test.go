package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/stationbrowser"
)

func TestBrowserReadRequiresSeparateOwnerGrant(t *testing.T) {
	dir := t.TempDir()
	app := New(dir)
	app.browserAvailable = func() bool { return true }
	initial, err := app.View()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := app.Save(initial.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}})
	if err != nil {
		t.Fatal(err)
	}
	req := observation(apphost.ObservationPageText, fixtureBV)
	app.readPage = func(ctx context.Context, target string, check func() error) (stationbrowser.Page, error) {
		t.Fatal("browser started without the separate grant")
		return stationbrowser.Page{}, nil
	}
	if _, err := app.ReadObservation(t.Context(), req); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("legacy metadata permission granted browser access: %v", err)
	}
	if _, err := app.Save(saved.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}, BrowserEnabled: true}); err != nil {
		t.Fatal(err)
	}
	app = New(dir)
	app.browserAvailable = func() bool { return true }
	app.readPage = func(ctx context.Context, target string, check func() error) (stationbrowser.Page, error) {
		if target != "https://www.bilibili.com/video/"+fixtureBV+"/" {
			t.Fatalf("wrong browser URL %q", target)
		}
		return stationbrowser.Page{Title: "合成页面标题", Description: "合成页面简介"}, check()
	}
	out, err := app.ReadObservation(t.Context(), req)
	if err != nil || len(out.Chunks) != 1 || !strings.Contains(out.Chunks[0].Text, "合成页面简介") ||
		!strings.Contains(out.Coverage, "独立 Edge") || len(out.Playback) != 0 || !out.Complete {
		t.Fatalf("browser evidence: %#v %v", out, err)
	}
}

func TestBrowserRevocationCancelsInFlightRead(t *testing.T) {
	app := New(t.TempDir())
	app.browserAvailable = func() bool { return true }
	initial, err := app.View()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := app.Save(initial.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}, BrowserEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	app.readPage = func(ctx context.Context, _ string, check func() error) (stationbrowser.Page, error) {
		if err := check(); err != nil {
			return stationbrowser.Page{}, err
		}
		close(started)
		<-ctx.Done()
		// A result arriving with the cancellation must still be discarded.
		return stationbrowser.Page{Title: "晚到的合成结果"}, nil
	}
	registry, err := apphost.New(app.Registration())
	if err != nil {
		t.Fatal(err)
	}
	observer := apphost.NewObserver(registry, t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	input := apphost.ObservationInput{AppID: ID, ConnectionID: "public", ContentID: fixtureBV, Field: apphost.ObservationPageText}
	finished := make(chan error, 1)
	go func() {
		_, err := observer.Read(ctx, input)
		finished <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("browser read never reached the fixture")
	}
	if _, err := app.Save(saved.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, apphost.ErrObservationForbidden) {
			t.Fatalf("late browser read returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal("revocation did not cancel the in-flight read")
	}
	for _, item := range observer.Discover(ctx) {
		for _, field := range item.Fields {
			if field == apphost.ObservationPageText {
				t.Fatal("revoked browser field still advertised")
			}
		}
	}
}
