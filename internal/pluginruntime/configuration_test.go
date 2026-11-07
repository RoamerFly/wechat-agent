package pluginruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/pluginhost"
)

type idleConfigurationSession struct {
	close func() error
	call  func(context.Context) (json.RawMessage, error)
}

// Done is reached by beginInvocation only after it has found the lifecycle
// reservation busy. This makes the interleaving observable without sleeps or
// production synchronization hooks.
type idleConfigurationWaitContext struct {
	context.Context
	entered   chan struct{}
	signalled atomic.Bool
}

func (c *idleConfigurationWaitContext) Done() <-chan struct{} {
	if c.signalled.CompareAndSwap(false, true) {
		close(c.entered)
	}
	return c.Context.Done()
}

func (*idleConfigurationSession) Health(ctx context.Context) error { return ctx.Err() }
func (s *idleConfigurationSession) Close() error {
	if s.close != nil {
		return s.close()
	}
	return nil
}
func (s *idleConfigurationSession) Call(ctx context.Context, _ pluginhost.Invocation, _ string, _ json.RawMessage) (json.RawMessage, error) {
	if s.call != nil {
		return s.call(ctx)
	}
	return json.RawMessage(`{}`), nil
}

func TestConfigureIdleRetainsEnableIntentAndReloadsOnlyOnDemand(t *testing.T) {
	s, i := installedFixture(t)
	before, err := s.SaveConfiguration(t.Context(), i.Package.ID, pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{"label": json.RawMessage(`"old"`)}, Secrets: map[string]string{"api_key": "synthetic-private-value"}})
	if err != nil {
		t.Fatal(err)
	}
	var starts, closes atomic.Int32
	var loaded []int
	c, err := NewController(s, startFunc(func(ctx context.Context, p pluginhost.Package, _ pluginhost.Grant) (pluginhost.Session, error) {
		starts.Add(1)
		config, e := s.ConfigurationForPlugin(ctx, p.ID)
		if e != nil {
			return nil, e
		}
		loaded = append(loaded, config.Revision)
		return &idleConfigurationSession{close: func() error { closes.Add(1); return nil }}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Start(t.Context(), i.Package.ID, i.Revision); err != nil {
		t.Fatal(err)
	}
	enabled, _ := s.Installed(t.Context(), i.Package.ID)
	after, err := c.ConfigureIdle(t.Context(), i.Package.ID, pluginhost.ConfigUpdate{Revision: before.Revision, Values: map[string]json.RawMessage{"label": json.RawMessage(`"new"`)}})
	if err != nil || after.Revision <= before.Revision || starts.Load() != 1 || closes.Load() != 1 || c.Status(i.Package.ID).State != "stopped" {
		t.Fatalf("configure: %+v %v starts=%d closes=%d status=%+v", after, err, starts.Load(), closes.Load(), c.Status(i.Package.ID))
	}
	retained, _ := s.Installed(t.Context(), i.Package.ID)
	if !retained.Enabled || retained.Revision != enabled.Revision {
		t.Fatal("configured tool lost enabled intent", retained)
	}
	private, err := s.ConfigurationForExecution(t.Context(), i.Package.ID)
	if err != nil || string(private.Values["api_key"]) != `"synthetic-private-value"` {
		t.Fatal("existing secret lost", err)
	}
	if err := c.EnsureStarted(t.Context(), i.Package.ID, retained.Revision); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 2 || len(loaded) != 2 || loaded[1] != after.Revision {
		t.Fatal("new runtime retained old config", loaded)
	}
	if _, err := c.ConfigureIdle(t.Context(), i.Package.ID, pluginhost.ConfigUpdate{Revision: before.Revision}); !errors.Is(err, pluginhost.ErrRevision) {
		t.Fatal("stale update", err)
	}
	if closes.Load() != 1 {
		t.Fatal("stale update stopped healthy session")
	}
	if _, err := c.ConfigureIdle(t.Context(), i.Package.ID, pluginhost.ConfigUpdate{Revision: after.Revision, Values: map[string]json.RawMessage{"unknown": json.RawMessage(`1`)}}); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	unchanged, err := s.Configuration(t.Context(), i.Package.ID)
	if err != nil || unchanged.Revision != after.Revision || string(unchanged.Values["label"]) != `"new"` {
		t.Fatal("invalid update mutated stored configuration", unchanged, err)
	}
	c.mu.Lock()
	_, stillLoaded := c.loaded[i.Package.ID]
	c.mu.Unlock()
	if closes.Load() != 2 || stillLoaded {
		t.Fatal("rejected update retained stale session", closes.Load(), stillLoaded)
	}
	if err := c.EnsureStarted(t.Context(), i.Package.ID, retained.Revision); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 3 || loaded[2] != after.Revision {
		t.Fatal("rejected update did not reload last valid configuration", loaded)
	}
}

func TestConfigureIdleRejectsRunningAndDurablePendingWork(t *testing.T) {
	s, i := installedFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	c, err := NewController(s, startFunc(func(context.Context, pluginhost.Package, pluginhost.Grant) (pluginhost.Session, error) {
		return &idleConfigurationSession{call: func(ctx context.Context) (json.RawMessage, error) {
			close(entered)
			select {
			case <-release:
				return json.RawMessage(`{}`), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Start(t.Context(), i.Package.ID, i.Revision); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := c.Invoke(t.Context(), CallRequest{Plugin: i.Package.ID, Capability: "echo", Context: invocation(), Arguments: json.RawMessage(`{}`)}, invocationPolicy{})
		done <- e
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("call did not start")
	}
	if _, err := c.ConfigureIdle(t.Context(), i.Package.ID, pluginhost.ConfigUpdate{}); err == nil {
		t.Fatal("active call was reconfigured")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	enabled, _ := s.Installed(t.Context(), i.Package.ID)
	config, _ := s.Configuration(t.Context(), i.Package.ID)
	in := invocation()
	in.RequestKey = "synthetic-pending-config"
	j, created, err := s.CreateJob(t.Context(), pluginhost.Job{Plugin: i.Package.ID, Version: enabled.Package.Version, PackageDigest: enabled.Digest, ConfigRevision: strconv.Itoa(config.Revision), Tool: "echo", Context: in, Arguments: json.RawMessage(`{}`)})
	if err != nil || !created {
		t.Fatal(err)
	}
	if _, err := c.ConfigureIdle(t.Context(), i.Package.ID, pluginhost.ConfigUpdate{Revision: config.Revision}); err == nil {
		t.Fatal("pending work was silently discarded")
	}
	remaining, err := s.Job(t.Context(), j.ID)
	if err != nil || remaining.State != "pending" || c.Status(i.Package.ID).State != "running" {
		t.Fatal("pending work/runtime changed", remaining, err)
	}
}

func TestConfigureIdleBlocksInvocationUntilOldSessionIsGone(t *testing.T) {
	s, i := installedFixture(t)
	closing, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c, err := NewController(s, startFunc(func(context.Context, pluginhost.Package, pluginhost.Grant) (pluginhost.Session, error) {
		return &idleConfigurationSession{close: func() error { close(closing); <-release; return nil }, call: func(context.Context) (json.RawMessage, error) { calls.Add(1); return json.RawMessage(`{}`), nil }}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Start(t.Context(), i.Package.ID, i.Revision); err != nil {
		t.Fatal(err)
	}
	configured := make(chan error, 1)
	go func() { _, e := c.ConfigureIdle(t.Context(), i.Package.ID, pluginhost.ConfigUpdate{}); configured <- e }()
	select {
	case <-closing:
	case <-time.After(5 * time.Second):
		t.Fatal("old session not closing")
	}
	invoked := make(chan error, 1)
	waiting := &idleConfigurationWaitContext{Context: t.Context(), entered: make(chan struct{})}
	go func() {
		_, e := c.Invoke(waiting, CallRequest{Plugin: i.Package.ID, Capability: "echo", Context: invocation(), Arguments: json.RawMessage(`{}`)}, invocationPolicy{})
		invoked <- e
	}()
	select {
	case <-waiting.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("invocation did not wait on lifecycle reservation")
	}
	select {
	case e := <-invoked:
		t.Fatal("invocation escaped lifecycle reservation", e)
	default:
	}
	close(release)
	if err := <-configured; err != nil {
		t.Fatal(err)
	}
	if err := <-invoked; err == nil {
		t.Fatal("old stopped runtime accepted invocation")
	}
	if calls.Load() != 0 {
		t.Fatal("call used stale session")
	}
	jobs, err := s.Jobs(t.Context(), i.Package.ID, 0, 10)
	if err != nil || len(jobs.Items) != 0 {
		t.Fatal("waiting invocation created work before rechecking runtime", jobs, err)
	}
}
