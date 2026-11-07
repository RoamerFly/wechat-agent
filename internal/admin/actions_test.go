package admin

import (
	"context"
	"encoding/json"
	"testing"
	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/linuxnative"
)

type actionNative struct {
	*fakeTools
	actions int
}

func (a *actionNative) ExecuteAction(_ context.Context, r linuxnative.AdminAction, key string) (linuxnative.ActionStatus, error) {
	a.actions++
	return linuxnative.ActionStatus{ID: "op-one", State: "running", Kind: r.Kind}, nil
}
func (a *actionNative) InspectAction(context.Context, string) (linuxnative.ActionStatus, error) {
	return linuxnative.ActionStatus{ID: "op-one", State: "succeeded"}, nil
}
func TestAdministrativeActionsRequirePrepareAndExactApproval(t *testing.T) {
	f := setup(t)
	native := &actionNative{fakeTools: &fakeTools{fakeNative: f.native}}
	f.manager.native = native
	f.login()
	c := defaults()
	c.GroupRules = []GroupRule{{ID: "room@chatroom", Mode: "mention", Name: "test"}}
	if _, err := f.manager.Save(ConfigInput{Config: c}); err != nil {
		t.Fatal(err)
	}
	request := linuxnative.AdminAction{Kind: "rename", ThreadID: "room@chatroom", Text: "new name"}
	code, data, _ := f.request("POST", "/api/actions/prepare", request, true, true)
	if code != 201 || native.actions != 0 {
		t.Fatal("prepare executed mutation", code)
	}
	var proposal wechatapp.ActionProposal
	if err := json.Unmarshal(data, &proposal); err != nil {
		t.Fatal(err)
	}
	route := "/api/actions/" + proposal.ID + "/approve"
	if code, _, _ := f.request("POST", route, map[string]string{"confirm": "wrong"}, true, true); code != 400 {
		t.Fatal("wrong confirmation accepted")
	}
	if code, _, _ := f.request("POST", route, map[string]string{"confirm": proposal.ID}, true, false); code != 403 {
		t.Fatal("CSRF absent")
	}
	for i := 0; i < 2; i++ {
		if code, _, _ := f.request("POST", route, map[string]string{"confirm": proposal.ID}, true, true); code != 202 && code != 200 {
			t.Fatal("approval failed", code)
		}
	}
	if native.actions != 1 {
		t.Fatal("approval replay repeated action")
	}
	if code, _, _ := f.request("GET", "/api/actions/"+proposal.ID, nil, false, false); code != 200 {
		t.Fatal("operation read failed")
	}
	persisted, err := wechatapp.OpenActionLedger(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := persisted.Read(proposal.ID, "test")
	if !ok || saved.State != "succeeded" {
		t.Fatal("operation result not durable")
	}
	request.ThreadID = "unapproved@chatroom"
	if code, _, _ := f.request("POST", "/api/actions/prepare", request, true, true); code != 403 {
		t.Fatal("unauthorized target allowed")
	}
}
