"""Synthetic browser acceptance for the local OBS program-scene preview card.

It never connects to OBS.  The browser only receives a server-shaped JPEG data
URL, while all local-station routes still run through the real executable.
"""

import argparse
import json
from pathlib import Path
import re
import tempfile

from playwright.sync_api import expect, sync_playwright

from test_local_home_ui import assert_no_horizontal_overflow, start, stop


JPEG = "data:image/jpeg;base64,/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/2wBDAQkJCQwLDBgNDRgyIRwhMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjL/wAARCAACAAIDASIAAhEBAxEB/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/8QAtRAAAgEDAwIEAwUFBAQAAAF9AQIDAAQRBRIhMUEGE1FhByJxFDKBkaEII0KxwRVS0fAkM2JyggkKFhcYGRolJicoKSo0NTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqDhIWGh4iJipKTlJWWl5iZmqKjpKWmp6ipqrKztLW2t7i5usLDxMXGx8jJytLT1NXW19jZ2uHi4+Tl5ufo6erx8vP09fb3+Pn6/8QAHwEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoL/8QAtREAAgECBAQDBAcFBAQAAQJ3AAECAxEEBSExBhJBUQdhcRMiMoEIFEKRobHBCSMzUvAVYnLRChYkNOEl8RcYGRomJygpKjU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6goOEhYaHiImKkpOUlZaXmJmaoqOkpaanqKmqsrO0tba3uLm6wsPExcbHyMnK0tPU1dbX2Nna4uPk5ebn6Onq8vP09fb3+Pn6/9oADAMBAAIRAxEAPwDjKKKK+0POP//Z"


def fulfill(route, body, status=200):
    route.fulfill(status=status, content_type="application/json", body=json.dumps(body, ensure_ascii=False))


def tool(state):
    return {
        "id": "obs-preview", "name": "OBS 画面预览", "catalog_available": True,
        "runtime_ready": state["configured"], "installed": state["installed"], "enabled": state["enabled"],
        "revision": state["revision"], "state": "running" if state["active"] else "idle",
    }


def view(state, image=False):
    result = {
        "configured": state["configured"], "port": state["port"],
        "config_revision": state["config_revision"], "enabled": state["enabled"],
        "active": state["active"],
    }
    if image and state["active"]:
        result.update(scene="节目场景", observed_at="2030-01-02T03:04:05Z", image_data=JPEG)
    return result


def verify(binary, output):
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="synthetic-obs-ui-", dir=output) as directory:
        root = Path(directory).resolve()
        process, origin = start(binary, root / "host", root / "workspace")
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(channel="msedge", headless=True)
                context = browser.new_context(viewport={"width": 1100, "height": 800}, color_scheme="light")
                page = context.new_page()
                state = {"installed": False, "configured": False, "port": 4455, "config_revision": 0,
                         "enabled": False, "active": False, "revision": 1}
                writes = []
                fail_frame = False

                def tools_route(route):
                    if route.request.method == "GET":
                        fulfill(route, {"tools": [tool(state)]})
                        return
                    body = route.request.post_data_json
                    assert route.request.headers.get("x-csrf-token"), "project tool mutation needs CSRF"
                    assert body["revision"] == state["revision"]
                    if body["action"] == "install":
                        assert body["approve_artifacts"] is True
                        state.update(installed=True, revision=2)
                    elif body["action"] == "enable":
                        assert state["configured"] and body["approve_artifacts"] is False
                        state.update(enabled=True, revision=2)
                    elif body["action"] == "disable":
                        state.update(enabled=False, active=False, revision=3)
                    else:
                        raise AssertionError(f"unexpected generic action {body['action']}")
                    fulfill(route, {"tool": tool(state)})

                def preview_route(route):
                    nonlocal fail_frame
                    if route.request.method == "GET":
                        fulfill(route, view(state))
                        return
                    body = route.request.post_data_json
                    assert route.request.headers.get("x-csrf-token"), "OBS mutation needs CSRF"
                    writes.append(body)
                    action = body.get("action")
                    if action == "configure":
                        assert set(body) == {"action", "port", "password", "config_revision"}
                        assert body["password"] == "synthetic-obs-password"
                        assert body["config_revision"] == 0
                        state.update(configured=True, port=4456, config_revision=1)
                        fulfill(route, view(state))
                    elif action == "start":
                        assert set(body) == {"action"} and state["configured"] and state["enabled"]
                        state["active"] = True
                        fulfill(route, view(state, image=True))
                    elif action == "frame":
                        assert set(body) == {"action"} and state["active"]
                        if fail_frame:
                            state["active"] = False
                            fulfill(route, {"error": "合成 OBS 读取失败"}, 502)
                            return
                        fulfill(route, view(state, image=True))
                    elif action == "stop":
                        assert set(body) == {"action"}
                        state["active"] = False
                        fulfill(route, view(state))
                    else:
                        raise AssertionError(f"unexpected preview action {action}")

                page.route(re.compile(r".*/api/local/project-tools/obs-preview/preview$"), preview_route)
                page.route(re.compile(r".*/api/local/project-tools(?:/obs-preview)?$"), tools_route)
                page.goto(origin)
                page.locator("#login-password").fill("synthetic-home-owner")
                page.locator("#login-submit").click()
                page.locator("#initialize-button").click()
                page.locator("#project-tools-tab").click()
                page.get_by_role("button",name="管理OBS 画面预览",exact=True).click()
                panel = page.locator("#obs-preview-panel")
                expect(page.get_by_text("确认仅访问本机 OBS WebSocket", exact=True)).to_be_visible()
                page.get_by_label("确认仅访问本机 OBS WebSocket").check()
                page.get_by_role("button", name="安装").click()
                expect(panel.get_by_text("OBS 当前节目画面", exact=True)).to_be_visible()
                expect(page.get_by_role("button", name="启用")).to_be_disabled()
                panel.get_by_label("OBS 回环端口").fill("4456")
                panel.get_by_label("OBS WebSocket 密码").fill("synthetic-obs-password")
                with page.expect_response(lambda response: response.url.endswith("/preview") and response.request.method == "POST"):
                    panel.get_by_role("button", name="保存凭据").click()
                assert [item["action"] for item in writes] == ["configure"]
                assert "synthetic-obs-password" not in panel.inner_text()
                expect(page.get_by_role("button", name="启用")).to_be_enabled()
                page.get_by_role("button", name="启用").click()
                expect(panel.get_by_role("button", name="开始预览")).to_be_enabled()
                with page.expect_response(lambda response: response.url.endswith("/preview") and response.request.method == "POST"):
                    panel.get_by_role("button", name="开始预览").click()
                image = panel.locator("img.obs-preview-image")
                expect(image).to_have_attribute("src", re.compile(r"^blob:"))
                page.wait_for_function("document.querySelector('#obs-preview-panel img.obs-preview-image')?.naturalWidth > 0")
                page.wait_for_timeout(1650)
                assert [item["action"] for item in writes].count("frame") == 1
                panel.screenshot(path=str(output / "obs-preview-active-light.png"))
                assert_no_horizontal_overflow(page, "OBS preview desktop")
                page.set_viewport_size({"width": 390, "height": 760})
                page.emulate_media(color_scheme="dark")
                page.wait_for_function("document.querySelector('#obs-preview-panel img.obs-preview-image')?.naturalWidth > 0")
                panel.screenshot(path=str(output / "obs-preview-active-narrow.png"))
                assert_no_horizontal_overflow(page, "OBS preview narrow active")
                fail_frame = True
                page.wait_for_timeout(1650)
                expect(panel.locator("img.obs-preview-image")).to_have_count(0)
                expect(panel.get_by_text("合成 OBS 读取失败", exact=True)).to_be_visible()
                fail_frame = False
                with page.expect_response(lambda response: response.url.endswith("/preview") and response.request.method == "POST"):
                    panel.get_by_role("button", name="开始预览").click()
                expect(panel.locator("img.obs-preview-image")).to_have_count(1)
                page.locator("#tool-dialog-close").click()
                page.locator("#chat-tab").click()
                expect(page.locator("#chat-view")).to_be_visible()
                assert [item["action"] for item in writes].count("stop") == 1
                page.locator("#project-tools-tab").click()
                expect(panel.locator("img.obs-preview-image")).to_have_count(0)
                assert_no_horizontal_overflow(page, "OBS preview narrow")
                browser.close()
        finally:
            stop(process)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    arguments = parser.parse_args()
    verify(arguments.binary.resolve(), arguments.output.resolve())
