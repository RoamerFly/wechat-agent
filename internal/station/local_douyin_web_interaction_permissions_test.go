package station

import (
	"context"
	"testing"
)

func TestLocalDouyinWebInteractionPermissionsAreExplicitAndRevokeWithDependencies(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	_, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": 0, "approve_profile": true}, true)
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "confirm", "revision": view["revision"], "attempt_id": view["attempt_id"]}, true)
	if view["likes_enabled"] != false || view["replies_enabled"] != false {
		t.Fatalf("interaction permissions must default off: %v", view)
	}
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_interactions", "revision": view["revision"], "allow_likes": true}, true); code != 409 {
		t.Fatal("likes without video read scope were accepted")
	}
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "read_scope": "home_and_videos"}, true)
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_interactions", "revision": view["revision"], "allow_replies": true}, true); code != 409 {
		t.Fatal("replies without comment read were accepted")
	}
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_interactions", "revision": view["revision"], "allow_likes": true}, true)
	if view["likes_enabled"] != true || view["replies_enabled"] != false {
		t.Fatalf("likes must not also enable replies: %v", view)
	}
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "allow_comments": true}, true)
	if view["likes_enabled"] != true || view["comments_enabled"] != true {
		t.Fatalf("comment permission overwrote another interaction permission: %v", view)
	}
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_interactions", "revision": view["revision"], "allow_replies": true}, true)
	if view["likes_enabled"] != true || view["replies_enabled"] != true {
		t.Fatalf("reply permission was not independently saved: %v", view)
	}
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "allow_comments": false}, true)
	if view["comments_enabled"] != false || view["likes_enabled"] != true || view["replies_enabled"] != false {
		t.Fatalf("comment revocation must only revoke dependent replies: %v", view)
	}
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "read_scope": "home"}, true)
	if view["likes_enabled"] != false || view["replies_enabled"] != false {
		t.Fatalf("scope reduction must revoke interactions: %v", view)
	}

	// A withdrawal must remain durable while the browser service is stopping.
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "read_scope": "home_and_videos"}, true)
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_interactions", "revision": view["revision"], "allow_likes": true}, true)
	stopping, stop := context.WithCancel(context.Background())
	stop()
	m.ctx = stopping
	code, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_interactions", "revision": view["revision"], "allow_likes": false}, true)
	if code != 200 || view["likes_enabled"] != false {
		t.Fatalf("offline withdrawal failed: %d %v", code, view)
	}
}
