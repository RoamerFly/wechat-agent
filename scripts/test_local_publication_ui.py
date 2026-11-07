"""Windows publication workbench acceptance with a real local station and synthetic platform tasks."""

import argparse
import json
from pathlib import Path
import re
import tempfile
from urllib.parse import parse_qs, urlparse

from playwright.sync_api import expect, sync_playwright

from test_local_home_ui import assert_no_horizontal_overflow, fulfill_json, start, stop


def verify(binary, output):
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="synthetic-publication-ui-", dir=output) as directory:
        root = Path(directory).resolve()
        process, origin = start(binary, root / "host", root / "workspace")
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(channel="msedge", headless=True)
                context = browser.new_context(viewport={"width": 1280, "height": 860}, color_scheme="light")
                page = context.new_page()
                errors = []
                writes = []
                page.on("pageerror", lambda error: errors.append(str(error)))
                page.goto(origin)
                assert context.request.get(origin + "/api/local/publications?app_id=bilibili").status == 401
                page.locator("#login-password").fill("synthetic-home-owner")
                page.locator("#login-submit").click()
                page.locator("#initialize-button").click()
                page.locator("#chat-view").wait_for(state="visible")
                listing = context.request.get(origin + "/api/local/publications?app_id=bilibili").json()
                assert listing["tasks"]["items"] == [] and listing["targets"] == []

                connection_url = origin + "/api/apps/bilibili/connections"
                assert [item["id"] for item in context.request.get(connection_url).json()["items"]] == ["public"]
                ready = False
                job_ids = {name: key * 64 for name, key in zip(
                    ("draft", "upload", "unknown", "accepted", "later"), "abcde"
                )}

                def job(name, state, actions, stage=None, confirmed=0, remote=""):
                    item = {"job_id": job_ids[name], "state": state, "updated_at": "2026-11-06T11:00:00Z",
                            "available_actions": actions, "remote_id": remote}
                    if stage:
                        item["progress"] = {"stage": stage, "confirmed_parts": confirmed, "total_parts": 3}
                    return item

                entries = {
                    "draft": job("draft", "draft", ["cancel", "submit"]),
                    "upload": job("upload", "awaiting_receipt", ["status", "cancel", "resume"], "uploading", 1),
                    "unknown": job("unknown", "awaiting_receipt", ["status", "cancel", "reconcile"], "posting", 3),
                    "accepted": job("accepted", "accepted", ["status"], "accepted", 3, "BV1aB2cD3e4F"),
                    "later": job("later", "cancelled", []),
                }
                paused_upload = []
                authorization_writes = []
                oauth_writes = []
                oauth_authorization = None
                local_expiry = "2030-01-01T08:00"
                expected_expiry = page.evaluate("(value) => new Date(value).toISOString()", local_expiry)

                def publication_authorization(route):
                    if route.request.method == "GET":
                        if oauth_authorization is not None:
                            fulfill_json(oauth_authorization)(route)
                            return
                        route.continue_()
                        return
                    body = route.request.post_data_json
                    assert route.request.headers.get("x-csrf-token"), "CSRF header missing"
                    assert set(body) == {"action", "revision", "client_id", "app_secret", "access_token", "expires_at", "enabled"}
                    assert body["action"] == "verify"
                    assert body["client_id"] == "synthetic-fixture-app"
                    assert body["app_secret"] == "synthetic-fixture-secret"
                    assert body["access_token"] == "synthetic-fixture-token"
                    assert body["expires_at"] == expected_expiry and body["enabled"] is True
                    authorization_writes.append(body)
                    fulfill_json({"revision": "synthetic-authorization-revision", "state": "ready", "verified": True,
                                  "enabled": True, "expires_at": "2030-01-01T00:00:00Z"})(route)

                def publication_oauth(route):
                    nonlocal oauth_authorization
                    body = route.request.post_data_json
                    assert route.request.headers.get("x-csrf-token"), "CSRF header missing"
                    oauth_writes.append(body)
                    if body["action"] == "begin":
                        assert set(body) == {"action", "revision", "client_id", "app_secret", "redirect_uri", "enabled"}
                        assert body["revision"] == "unconfigured"
                        assert body["client_id"] == "synthetic-oauth-app"
                        assert body["app_secret"] == "synthetic-oauth-secret"
                        assert body["redirect_uri"] == "https://localhost.example.test/bilibili/callback"
                        assert body["enabled"] is True
                        fulfill_json({"authorize_url": "https://account.bilibili.com/pc/account-pc/auth/oauth?state=synthetic",
                                      "expires_at": "2030-01-01T00:10:00Z"})(route)
                    elif body["action"] == "complete":
                        assert set(body) == {"action", "callback_url"}
                        assert body["callback_url"] == "https://localhost.example.test/bilibili/callback?code=transient&state=synthetic"
                        oauth_authorization = {"revision": "synthetic-oauth-complete", "state": "ready", "verified": True,
                                               "enabled": True, "expires_at": "2030-02-01T00:00:00Z", "refresh_available": True,
                                               "refresh_pending": False}
                        fulfill_json(oauth_authorization)(route)
                    elif body["action"] == "refresh":
                        assert set(body) == {"action", "revision"}
                        assert body["revision"] == "synthetic-oauth-complete"
                        oauth_authorization = {"revision": "synthetic-oauth-refreshed", "state": "ready", "verified": True,
                                               "enabled": True, "expires_at": "2030-03-01T00:00:00Z", "refresh_available": False,
                                               "refresh_pending": True}
                        fulfill_json({"error": "续期结果未确认，请重新网页授权"}, 502)(route)
                    else:
                        raise AssertionError(f"unexpected OAuth action {body['action']}")

                def publications(route):
                    request = route.request
                    if request.method == "GET":
                        params = parse_qs(urlparse(request.url).query)
                        assert params["app_id"] == ["bilibili"] and params["limit"] == ["6"]
                        after = int(params["after"][0])
                        items = [] if not ready else [entries["later"]] if after else [
                            entries[name] for name in ("draft", "upload", "unknown", "accepted")
                        ]
                        fulfill_json({"tasks": {"items": items, "next": 42, "has_more": ready and not after}})(route)
                        return
                    body = request.post_data_json
                    assert request.headers.get("x-csrf-token"), "CSRF header missing"
                    assert set(body) == {"action", "job_id"}
                    name = next(key for key, value in job_ids.items() if value == body["job_id"])
                    action = body["action"]
                    writes.append((name, action))
                    if name == "draft" and action == "submit":
                        entries[name] = job(name, "accepted", ["status"], "accepted", 3, "BV9aB2cD3e4F")
                    elif name == "upload" and action == "resume":
                        paused_upload.append(route)
                        return
                    elif name == "upload" and action == "cancel":
                        entries[name] = job(name, "cancelled", [], "uploading", 1)
                        fulfill_json(entries[name])(paused_upload.pop())
                    elif name == "unknown" and action == "status":
                        fulfill_json({"error": "测试回执服务暂不可用"}, 503)(route)
                        return
                    elif name == "unknown" and action == "cancel":
                        entries[name].update(cancel_requested=True, available_actions=["status", "reconcile"])
                    elif name == "accepted" and action == "status":
                        entries[name] = job(name, "published", [], "accepted", 3, "BV1aB2cD3e4F")
                    else:
                        raise AssertionError(f"unexpected publication action {name}/{action}")
                    fulfill_json(entries[name])(route)

                reconciliation_writes = []

                def publication_reconciliation(route):
                    request = route.request
                    assert request.method == "POST"
                    assert request.headers.get("x-csrf-token"), "CSRF header missing"
                    body = request.post_data_json
                    reconciliation_writes.append(body)
                    assert body["job_id"] == job_ids["unknown"]
                    if body["action"] == "candidates":
                        assert set(body) == {"job_id", "action", "page", "page_size"}
                        assert body["page_size"] == 10
                        if body["page"] == 1:
                            fulfill_json({"job_id": job_ids["unknown"], "items": [
                                {"bvid": "BV1aB2cD3e4F", "title": "候选稿件", "created_at": "2026-11-06T11:00:00Z"}
                            ], "page": 1, "page_size": 10, "has_more": True, "next_page": 2})(route)
                            return
                        assert body["page"] == 2
                        fulfill_json({"job_id": job_ids["unknown"], "items": [], "page": 2, "page_size": 10, "has_more": False})(route)
                        return
                    if body["action"] == "verify":
                        assert set(body) == {"job_id", "action", "bvid"}
                        assert body["bvid"] == "BV1aB2cD3e4F"
                        entries["unknown"]["reconciliation"] = {"bvid": body["bvid"], "published": False,
                                                              "checked_at": "2026-11-06T12:00:00Z"}
                        fulfill_json({"job_id": job_ids["unknown"], "item": entries["unknown"]["reconciliation"]})(route)
                        return
                    raise AssertionError(f"unexpected reconciliation action {body['action']}")

                page.route(re.compile(r".*/api/local/publications(?:\?.*)?$"), publications)
                page.route(re.compile(r".*/api/local/publications/reconcile$"), publication_reconciliation)
                page.route(re.compile(r".*/api/local/apps/bilibili/publication-authorization$"), publication_authorization)
                page.route(re.compile(r".*/api/local/apps/bilibili/publication-oauth$"), publication_oauth)
                page.locator("#apps-tab").click()
                panel = page.locator("#bilibili-publication")
                expect(panel.get_by_text("尚未登记发布授权", exact=True)).to_be_visible()
                authorization = panel.locator(".publication-authorization")
                authorization.locator("summary").click()
                authorization.screenshot(path=str(output / "bilibili-authorization-light.png"))
                authorization.get_by_label("Client ID").fill("synthetic-fixture-app")
                authorization.get_by_label("App Secret").fill("synthetic-fixture-secret")
                authorization.get_by_label("Access Token").fill("synthetic-fixture-token")
                authorization.get_by_label("凭据到期时间").fill(local_expiry)
                authorization.get_by_label("完成核验后启用发布授权").check()
                with page.expect_response(lambda r: r.url.endswith("/publication-authorization") and r.request.method == "POST"):
                    authorization.get_by_role("button", name="核验并登记").click()
                assert len(authorization_writes) == 1
                expect(page.locator("#apps-list[aria-busy='false']")).to_be_visible()
                expect(panel.get_by_text("尚未登记发布授权", exact=True)).to_be_visible()
                expect(panel.get_by_text("暂无 B 站发布任务。", exact=True)).to_be_visible()
                expect(panel.get_by_role("button", name="提交投稿")).to_have_count(0)
                oauth = panel.locator(".publication-oauth")
                oauth.locator("summary").click()
                oauth.get_by_label("OAuth Client ID").fill("synthetic-oauth-app")
                oauth.get_by_label("OAuth App Secret").fill("synthetic-oauth-secret")
                oauth.get_by_label("回调地址", exact=True).fill("https://localhost.example.test/bilibili/callback")
                oauth.get_by_label("完成授权后启用发布授权").check()
                with page.expect_response(lambda r: r.url.endswith("/publication-oauth") and r.request.method == "POST"):
                    oauth.get_by_role("button", name="生成授权链接").click()
                oauth_link = oauth.get_by_role("link", name="在浏览器中打开 B 站授权")
                expect(oauth_link).to_have_attribute("href", re.compile(r"^https://account\.bilibili\.com/pc/account-pc/auth/oauth"))
                expect(oauth_link).to_have_attribute("rel", "noopener noreferrer")
                oauth.screenshot(path=str(output / "bilibili-oauth-light.png"))
                oauth.get_by_label("完整回调地址").fill("https://localhost.example.test/bilibili/callback?code=transient&state=synthetic")
                with page.expect_response(lambda r: r.url.endswith("/publication-oauth") and r.request.method == "POST"):
                    oauth.get_by_role("button", name="完成网页授权").click()
                expect(panel.get_by_role("button", name="手动续期")).to_be_enabled()
                with page.expect_response(lambda r: r.url.endswith("/publication-oauth") and r.request.method == "POST"):
                    panel.get_by_role("button", name="手动续期").click()
                expect(panel.get_by_text("续期结果未确认", exact=True)).to_be_visible()
                assert [write["action"] for write in oauth_writes] == ["begin", "complete", "refresh"]
                oauth_authorization = None
                settings = root / "host" / "apps" / "bilibili" / "publication-settings.json"
                settings.parent.mkdir(parents=True, exist_ok=True)
                settings.write_text(json.dumps({
                    "schema_version": 1, "enabled": True, "client_id": "synthetic-fixture-app",
                    "app_secret": "synthetic-fixture-secret", "access_token": "synthetic-fixture-token",
                    "account_id": "synthetic-fixture-account", "expires_at": "2030-01-01T00:00:00Z",
                }), encoding="utf-8")
                grant = context.request.get(connection_url)
                assert grant.ok and "synthetic-fixture-" not in grant.text()
                assert [(item["id"], item["configuration"]) for item in grant.json()["items"] if item["id"] == "publish"] == [("publish", "ready")]
                ready = True
                page.locator("#apps-refresh").click()
                expect(panel.get_by_text("旧版发布授权待升级", exact=True)).to_be_visible()
                expect(panel.get_by_role("progressbar", name="已确认 1 / 3 段")).to_be_visible()
                unknown = panel.locator(f'[data-job="{job_ids["unknown"]}"]')
                expect(unknown.get_by_text("投稿结果待核对", exact=True)).to_be_visible()
                expect(unknown.get_by_role("button", name="继续任务")).to_have_count(0)
                unknown.get_by_text("核对投稿结果", exact=True).click()
                known_bvid = unknown.get_by_label(f'任务 {job_ids["unknown"][:10]} 的 BV 号')
                known_bvid.fill("BV-not-valid")
                unknown.get_by_role("button", name="核对 BV 号").click()
                expect(unknown.get_by_text("请输入完整的 BV 号后再核对。", exact=True)).to_be_visible()
                unknown.get_by_role("button", name="查找候选稿件").click()
                expect(unknown.get_by_text("BV1aB2cD3e4F · 候选稿件", exact=False)).to_be_visible()
                unknown.get_by_role("button", name="继续查找第 2 页").click()
                expect(unknown.get_by_text("暂未找到可核对的候选稿件。", exact=True)).to_be_visible()
                unknown.get_by_role("button", name="查找候选稿件").click()
                expect(unknown.get_by_text("BV1aB2cD3e4F · 候选稿件", exact=False)).to_be_visible()
                unknown.get_by_role("button", name="核对此稿件").click()
                expect(unknown.get_by_text("已核对 BV 号 BV1aB2cD3e4F，平台尚未显示公开", exact=False)).to_be_visible()
                expect(unknown.get_by_text("投稿结果待核对", exact=True)).to_be_visible()
                page.locator("#apps-refresh").click()
                expect(page.locator("#apps-list[aria-busy='false']")).to_be_visible()
                expect(unknown.get_by_text("已核对 BV 号 BV1aB2cD3e4F，平台尚未显示公开", exact=False)).to_be_visible()
                panel.screenshot(path=str(output / "bilibili-publication-light.png"))

                def row(name):
                    return panel.locator(f'[data-job="{job_ids[name]}"]')

                row("draft").get_by_role("button", name="提交投稿").click()
                expect(row("draft").get_by_text("已受理", exact=True)).to_be_visible()
                row("upload").get_by_role("button", name="继续任务").click()
                expect(row("upload").get_by_text("正在处理原任务…")).to_be_visible()
                row("upload").get_by_role("button", name="停止后续").click()
                expect(row("upload").get_by_text("已停止", exact=True)).to_be_visible()
                unknown.get_by_role("button", name="查询状态").click()
                expect(panel.get_by_role("status")).to_have_text("测试回执服务暂不可用")
                unknown.get_by_role("button", name="停止后续").click()
                expect(unknown.get_by_text("已要求停止", exact=True)).to_be_visible()
                expect(unknown.get_by_role("button", name="停止后续")).to_have_count(0)
                expect(unknown.get_by_text("核对投稿结果", exact=True)).to_be_visible()
                row("accepted").get_by_role("button", name="查询状态").click()
                expect(row("accepted").get_by_text("已公开", exact=True)).to_be_visible()
                panel.get_by_role("button", name="下一页").click()
                expect(panel.get_by_text("第 2 页", exact=True)).to_be_visible()
                expect(row("later")).to_be_visible()
                panel.get_by_role("button", name="上一页").click()
                expect(panel.get_by_text("第 1 页", exact=True)).to_be_visible()
                expect(row("accepted")).to_be_visible()
                page.set_viewport_size({"width": 390, "height": 844})
                page.emulate_media(color_scheme="dark")
                assert_no_horizontal_overflow(page, "B 站发布工作台")
                assert panel.evaluate("""panel => {
                    const {left, right} = panel.getBoundingClientRect();
                    return Array.from(panel.querySelectorAll('button, progress')).every(node => {
                        const rect = node.getBoundingClientRect();
                        return !node.getClientRects().length || (rect.left >= left && rect.right <= right);
                    });
                }"""), "controls clipped by application card"
                panel.screenshot(path=str(output / "bilibili-publication-dark-390.png"))
                authorization.locator("summary").click()
                assert_no_horizontal_overflow(page, "B 站授权登记")
                authorization.screenshot(path=str(output / "bilibili-authorization-dark-390.png"))
                authorization.locator("summary").click()
                oauth.locator("summary").click()
                assert_no_horizontal_overflow(page, "B 站网页授权")
                oauth.screenshot(path=str(output / "bilibili-oauth-dark-390.png"))
                assert not errors, errors
                assert writes == [("draft", "submit"), ("upload", "resume"), ("upload", "cancel"),
                                  ("unknown", "status"), ("unknown", "cancel"), ("accepted", "status")], writes
                assert len(authorization_writes) == 1
                assert [write["action"] for write in reconciliation_writes] == ["candidates", "candidates", "candidates", "verify"]
                browser.close()
        finally:
            stop(process)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    verify(args.binary.resolve(), args.output.resolve())
    print("Bilibili publication workbench acceptance passed")
