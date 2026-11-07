"""Synthetic interaction UI replay against a real local host; no platform/model calls."""

import argparse
import json
import os
from pathlib import Path
import tempfile
from urllib.parse import parse_qs, urlparse

from playwright.sync_api import expect, sync_playwright

from test_local_home_ui import (
    assert_no_horizontal_overflow, fulfill_json, home_tab, open_application, start, stop,
)


def verify(binary, output):
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="synthetic-interactions-", dir=output) as directory:
        root = Path(directory).resolve()
        process, origin = start(binary, root / "host", root / "workspace")
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(channel="msedge", headless=True)
                for width, color, suffix in ((1280, "light", "desktop-light"), (375, "dark", "mobile-dark")):
                    context = browser.new_context(viewport={"width": width, "height": 900}, color_scheme=color)
                    page = context.new_page()
                    page.add_init_script("""(() => {
                        const fetch = window.fetch.bind(window);
                        window.fixtureHold = false;
                        window.fetch = (...args) => {
                            const url = typeof args[0] === 'string' ? args[0] : args[0]?.url || '';
                            const response = fetch(...args);
                            if (!window.fixtureHold || !url.includes('/api/local/interactions?')) return response;
                            window.fixtureHold = false;
                            return response.then(value => new Promise(resolve => {
                                window.fixtureRelease = () => resolve(value);
                            }));
                        };
                    })();""")
                    errors, external, reads, posts, publication_reads = [], [], [], [], []
                    page.on("pageerror", lambda error: errors.append(str(error)))
                    items = []
                    for number, state in enumerate(("pending", "unknown", "applied", "failed", "cancelled", "unknown"), 1):
                        items.append({"job_id": f"{number:064x}", "state": state,
                                      "kind": "reply_comment" if number == 2 else "like", "desired": number != 3,
                                      "cancel_requested": number in (3, 6), "updated_at": "2030-01-02T03:04:05Z",
                                      "available_actions": ["status", "cancel"] if number == 1 else ["status"] if number == 2 else [],
                                      "diagnostic": "receipt_unknown" if number == 2 else ""})
                    second = {**items[0], "job_id": f"{7:064x}", "state": "cancelled", "available_actions": []}
                    deny_action, late_401 = False, False
                    permission_error = "合成互动许可已撤回，请重新检查许可。"

                    def intercept(route):
                        nonlocal late_401
                        request = route.request
                        parsed = urlparse(request.url)
                        path = parsed.path
                        if not request.url.startswith(origin + "/"):
                            external.append(request.url)
                            route.abort()
                            return
                        if path == "/api/local/interactions":
                            if request.method == "GET":
                                query = parse_qs(parsed.query)
                                assert query["app_id"] == ["douyin"] and query["limit"] == ["6"]
                                after = int(query["after"][0])
                                assert after in (0, 6)
                                reads.append(after)
                                if late_401:
                                    late_401 = False
                                    fulfill_json({"error": "合成迟到登录失效"}, 401)(route)
                                else:
                                    fulfill_json({"tasks": {"items": items if after == 0 else [second],
                                                           "next": 6 if after == 0 else 0,
                                                           "has_more": after == 0, "limit": 6}, "targets": []})(route)
                                return
                            assert request.method == "POST"
                            body = request.post_data_json
                            assert set(body) == {"action", "job_id"}
                            assert body["action"] in ("status", "cancel")
                            assert body["job_id"] in (items[0]["job_id"], items[1]["job_id"])
                            posts.append(body)
                            if deny_action:
                                fulfill_json({"error": permission_error}, 403)(route)
                            else:
                                item = next(item for item in items if item["job_id"] == body["job_id"])
                                if body["action"] == "cancel":
                                    item.update(state="applied", cancel_requested=True, available_actions=[])
                                fulfill_json(item)(route)
                            return
                        if path == "/api/local/publications":
                            assert request.method == "GET", "publication actions must not be invoked"
                            publication_reads.append(request.url)
                            fulfill_json({"tasks": {"items": [], "next": 0, "has_more": False}, "targets": []})(route)
                        elif path == "/api/local/apps/catalog":
                            fulfill_json({"applications": [{"id": "douyin", "name": "抖音", "install_state": "installed", "source_available": True}]})(route)
                        elif path == "/api/apps":
                            fulfill_json({"items": [{"id": "douyin", "name": "抖音"}]})(route)
                        elif path == "/api/apps/douyin/connections":
                            fulfill_json({"items": []})(route)
                        elif path == "/api/local/apps/douyin/web-login":
                            assert request.method == "GET"
                            fulfill_json({"revision": 1, "state": "saved", "active": False, "available": True,
                                          "read_enabled": True, "read_scope": "home_and_videos",
                                          "likes_enabled": True, "comments_enabled": True, "replies_enabled": True})(route)
                        elif "douyin" in path or path.startswith("/api/local/models"):
                            raise AssertionError(f"unexpected application/model request: {request.method} {path}")
                        else:
                            route.continue_()

                    page.route("**/*", intercept)
                    page.goto(origin)
                    page.locator("#login-password").fill("synthetic-home-owner")
                    page.locator("#login-submit").click()
                    page.locator("#home-view").wait_for(state="visible")
                    if page.locator("#initialize-button").is_visible():
                        page.locator("#initialize-button").click()
                        page.locator("#initialize-button").wait_for(state="hidden")
                    home_tab(page, "apps")
                    page.locator("#apps-list[aria-busy='false']").wait_for()
                    open_application(page, "抖音")
                    page.get_by_role("button", name="发布记录", exact=True).wait_for()
                    assert publication_reads and not reads and not posts, "interactions must load only on selection"
                    page.get_by_role("button", name="互动记录", exact=True).click()
                    listing = page.get_by_role("list", name="抖音互动记录", exact=True)
                    expect(listing.locator("li")).to_have_count(6)

                    def row(number):
                        return listing.locator(f'[data-job="{number:064x}"]')

                    for number, label in ((1, "待处理"), (2, "待核对"), (3, "已完成"), (4, "未完成"), (5, "已停止"), (6, "正在停止")):
                        expect(row(number).get_by_text(label, exact=True)).to_be_visible()
                    expect(row(3).get_by_text("取消点赞", exact=True)).to_be_visible()
                    assert "正在停止" not in row(3).inner_text()
                    assert "停止请求未撤回" in row(3).inner_text()
                    page.locator('[data-page="next"]').click()
                    expect(row(7)).to_be_visible()
                    expect(page.get_by_text("第 2 页", exact=True)).to_be_visible()
                    page.locator('[data-page="previous"]').click()
                    expect(row(1)).to_be_visible()
                    assert reads[:3] == [0, 6, 0]
                    row(2).locator('[data-action="status"]').click()
                    expect(row(2).locator('[data-action="status"]')).to_be_enabled()
                    assert posts == [{"action": "status", "job_id": items[1]["job_id"]}]
                    row(1).locator('[data-action="cancel"]').click()
                    expect(row(1).get_by_text("已完成", exact=True)).to_be_visible()
                    assert "正在停止" not in row(1).inner_text()
                    assert posts[-1] == {"action": "cancel", "job_id": items[0]["job_id"]}
                    deny_action = True
                    before = len(reads)
                    row(2).locator('[data-action="status"]').click()
                    expect(page.get_by_text(permission_error, exact=True)).to_be_visible()
                    expect(row(2).locator('[data-action="status"]')).to_be_enabled()
                    assert len(reads) > before, "403 feedback must survive the following successful GET"
                    expect(page.get_by_text(permission_error, exact=True)).to_be_visible()
                    expect(page.locator("#home-view")).to_be_visible()
                    assert_no_horizontal_overflow(page, suffix)
                    page.screenshot(path=str(output / f"interactions-{suffix}.png"), full_page=True)
                    late_401 = True
                    page.evaluate("window.fixtureHold = true")
                    page.locator('[data-page="next"]').click()
                    page.wait_for_function("typeof window.fixtureRelease === 'function'")
                    page.locator("#app-dialog-close").click()
                    closed_dom = page.locator("#app-dialog").inner_html()
                    page.evaluate("window.fixtureRelease()")
                    page.wait_for_timeout(150)
                    expect(page.locator("#home-view")).to_be_visible()
                    expect(page.locator("#app-dialog")).not_to_be_visible()
                    assert page.locator("#app-dialog").inner_html() == closed_dom, "late 401 repainted closed detail"
                    assert not errors and not external, (errors, external)
                    context.close()
                browser.close()
        finally:
            stop(process)
    print(json.dumps({"status": "passed", "synthetic": True, "platform_calls": 0, "model_calls": 0,
                      "screenshots": ["interactions-desktop-light.png", "interactions-mobile-dark.png"]}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    os.environ["PYTHONDONTWRITEBYTECODE"] = "1"
    verify(args.binary.resolve(), args.output.resolve())
