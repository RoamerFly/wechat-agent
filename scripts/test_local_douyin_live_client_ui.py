"""Synthetic owner-selected client inspection UI; no native client or platform is opened."""

import argparse
import json
from pathlib import Path
import re
import tempfile

from playwright.sync_api import expect, sync_playwright

from test_local_home_ui import assert_no_horizontal_overflow, fulfill_json, home_tab, open_application, start, stop


def verify(binary, output):
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="synthetic-live-client-", dir=output) as directory:
        root = Path(directory)
        process, origin = start(binary, root / "host", root / "workspace")
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(channel="msedge", headless=True)
                for width, color in ((1280, "light"), (375, "dark")):
                    context = browser.new_context(viewport={"width": width, "height": 800}, color_scheme=color)
                    page = context.new_page()
                    page.add_init_script("""(() => {
                      const original = fetch.bind(window);
                      window.delayClientInspection = false;
                      window.fetch = (...args) => {
                        const result = original(...args);
                        return String(args[0]).endsWith('/live-client/inspect') && window.delayClientInspection
                          ? result.then(value => new Promise(resolve => setTimeout(() => resolve(value), 600))) : result;
                      };
                    })();""")
                    page.route(re.compile(r".*/api/local/apps/catalog$"), fulfill_json({"applications": [{"id": "douyin", "name": "抖音", "install_state": "installed"}]}))
                    page.route(re.compile(r".*/api/apps$"), fulfill_json({"items": [{"id": "douyin", "name": "抖音"}]}))
                    page.route("**/api/apps/douyin/connections", fulfill_json({"items": []}))
                    page.route("**/api/local/apps/douyin/web-login", fulfill_json({"revision": 0, "state": "unconfigured", "active": False, "available": False}))
                    page.route(re.compile(r".*/api/local/publications\?app_id=douyin.*$"), fulfill_json({"tasks": {"items": [], "has_more": False}, "targets": []}))
                    calls = []
                    payload = {"state": "inspected", "control_available": False,
                               "file": {"product_name": "合成客户端", "version": "1.2.3.4", "architecture": "x64"},
                               "reason": "已读取程序版本及指纹；发布者、客户端登录和直播控制仍待核对"}
                    status = 200

                    def inspect(route):
                        calls.append(route.request.post_data_json)
                        fulfill_json(payload, status)(route)

                    page.route("**/api/local/apps/douyin/live-client/inspect", inspect)
                    page.goto(origin)
                    page.locator("#login-password").fill("synthetic-home-owner")
                    page.locator("#login-submit").click()
                    page.locator("#home-view").wait_for(state="visible")
                    if page.locator("#initialize-button").is_visible():
                        page.locator("#initialize-button").click()
                        page.locator("#initialize-button").wait_for(state="hidden")
                    home_tab(page, "apps")
                    open_application(page, "抖音")
                    form = page.get_by_role("form", name="直播伴侣程序检查")
                    path = page.get_by_role("textbox", name="直播伴侣主程序路径")
                    button = form.get_by_role("button", name="检查程序", exact=True)
                    expect(button).to_be_disabled()
                    assert not calls, "opening the detail must not inspect or start programs"
                    path.fill(r"E:\synthetic\直播伴侣.exe")
                    button.click()
                    expect(form).to_contain_text("合成客户端 · 1.2.3.4 · x64")
                    expect(form).to_contain_text("仍待核对")
                    assert calls == [{"executable": r"E:\synthetic\直播伴侣.exe"}]
                    # Editing a draft invalidates the old result; refreshing keeps the draft.
                    path.fill(r"E:\synthetic\other.exe")
                    expect(form).not_to_contain_text("1.2.3.4")
                    page.locator("#app-dialog-refresh").click()
                    expect(path).to_have_value(r"E:\synthetic\other.exe")
                    payload = {"state": "metadata_missing", "file": {"architecture": "x64"}, "control_available": False,
                               "reason": "版本资料不完整，请确认选中了直播伴侣主程序"}
                    button.click()
                    expect(form).to_contain_text("版本资料不完整")
                    status, payload = 403, {"error": "本次访问未获准"}
                    button.click()
                    expect(form).to_contain_text("本次访问未获准")
                    expect(page.locator("#home-view")).to_be_visible()
                    status, payload = 200, {"state": "inspected", "control_available": True}
                    button.click()
                    expect(form).to_contain_text("程序检查返回不完整")
                    form.scroll_into_view_if_needed()
                    assert_no_horizontal_overflow(page, f"client inspect {width}")
                    assert form.evaluate("e => e.scrollWidth <= e.clientWidth + 1"), "client form overflows"
                    page.locator("#app-dialog").screenshot(path=str(output / f"live-client-{width}-{color}.png"))
                    # A closed dialog discards a late authentication error and cancels its fetch.
                    page.evaluate("window.delayClientInspection = true")
                    status, payload = 401, {"error": "synthetic late authentication"}
                    button.click()
                    expect(form.get_by_role("button", name="检查中…", exact=True)).to_be_disabled()
                    expect(path).to_be_disabled()
                    page.locator("#app-dialog-close").click()
                    page.wait_for_timeout(750)
                    open_application(page, "抖音")
                    expect(page.locator("#home-view")).to_be_visible()
                    expect(path).to_have_value(r"E:\synthetic\other.exe")
                    expect(form).not_to_contain_text("synthetic late authentication")
                    context.close()
                browser.close()
        finally:
            stop(process)
    print("Client inspection UI replay passed: local synthetic responses only.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    verify(args.binary.resolve(), args.output.resolve())
