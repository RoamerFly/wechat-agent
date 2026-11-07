"""Synthetic owner OBS setup replay. Never installs or starts actual OBS."""

import argparse
import json
from pathlib import Path
import tempfile
from urllib.parse import urlparse

from playwright.sync_api import expect, sync_playwright

from test_local_home_ui import assert_no_horizontal_overflow, start, stop


def verify(binary, output):
    output.mkdir(parents=True, exist_ok=True)
    endpoint = "/api/local/project-tools/obs-preview/deployment"
    initial = {"revision": "missing", "version": "33.0.1", "archive_sha256": "0d9436451c2d772b33428e3fdae9776f071895c335de17283176306fb72651d3",
               "state": "missing", "installed": False, "read_only": False, "restart_required": False}
    with tempfile.TemporaryDirectory(prefix="synthetic-obs-setup-", dir=output) as directory:
        root = Path(directory).resolve()
        process, origin = start(binary, root / "host", root / "workspace")
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(channel="msedge", headless=True)
                page = browser.new_page(viewport={"width": 1120, "height": 820}, color_scheme="light")
                page.add_init_script("""(() => {
                  const original = window.fetch.bind(window);
                  window.fixtureHold = false;
                  window.fetch = (...args) => {
                    const response = original(...args);
                    if (!window.fixtureHold || !String(args[0]).endsWith('/deployment')) return response;
                    window.fixtureHold = false; window.fixtureRelease = null;
                    return response.then(value => new Promise(resolve => { window.fixtureRelease = () => resolve(value); }));
                  };
                })();""")
                server, mode, writes, errors, unexpected = dict(initial), "normal", [], [], []
                page.on("pageerror", lambda error: errors.append(str(error)))

                def fulfill(route, value, status=200):
                    route.fulfill(status=status, content_type="application/json", body=json.dumps(value, ensure_ascii=False))

                def intercept(route):
                    nonlocal server
                    request, path = route.request, urlparse(route.request.url).path
                    if not request.url.startswith(origin + "/"):
                        unexpected.append(request.url)
                        route.abort()
                    elif path == "/api/local/project-tools":
                        assert request.method == "GET", "setup must not install/enable the separate preview tool"
                        fulfill(route, {"tools": [{"id": item, "name": name, "catalog_available": True, "runtime_ready": True,
                                                 "installed": False, "enabled": False, "revision": 1, "state": "idle"}
                                                for item, name in [("obs-preview", "OBS 节目预览"), ("audio-workshop", "音频处理")]]})
                    elif path == endpoint:
                        if request.method == "GET":
                            fulfill(route, {"error": "synthetic old login"} if mode == "late401" else ({**server, "version": "wrong"} if mode == "bad_get" else server), 401 if mode == "late401" else 200)
                            return
                        assert request.method == "PUT" and request.headers.get("x-csrf-token")
                        body = request.post_data_json
                        writes.append(body)
                        assert body == {"revision": "missing", "archive_path": r"E:\Synthetic\OBS.zip", "approved": True}
                        if mode == "conflict":
                            fulfill(route, {"error": "合成安装正在处理中"}, 409)
                        elif mode == "forbidden":
                            fulfill(route, {"error": "synthetic forbidden"}, 403)
                        else:
                            server = {**initial, "revision": "present", "state": "installed", "installed": True,
                                      "read_only": True, "restart_required": True, "reason": "已安装，重启应用后启用专用节目输出；不会自动开播。"}
                            fulfill(route, server)
                    else:
                        route.continue_()

                page.route("**/*", intercept)
                page.goto(origin)
                page.locator("#login-password").fill("synthetic-home-owner")
                page.locator("#login-submit").click()
                page.locator("#initialize-button").click()
                page.locator("#project-tools-tab").click()
                page.get_by_role("button", name="管理OBS 节目预览", exact=True).click()
                dialog, save = page.locator("#tool-dialog"), page.locator("#local-deployment-save")
                path, approval = page.locator("#obs-output-archive-path"), page.locator("#obs-output-approved")
                expect(path).to_be_visible()
                path.fill(r"E:\Synthetic\OBS.zip")
                path.press("Enter")
                expect(save).to_be_disabled()
                assert not writes
                approval.check()
                path.fill(r"E:\Synthetic\OBS-edited.zip")
                expect(approval).not_to_be_checked()
                path.fill(r"E:\Synthetic\OBS.zip")
                approval.check()
                dialog.screenshot(path=str(output / "obs-setup-desktop-light.png"))
                page.set_viewport_size({"width": 375, "height": 820})
                page.emulate_media(color_scheme="dark")
                approval.scroll_into_view_if_needed()
                assert_no_horizontal_overflow(page, "OBS setup narrow dark")
                box = approval.bounding_box()
                assert box and 14 <= box["width"] <= 20
                dialog.screenshot(path=str(output / "obs-setup-narrow-dark.png"))
                mode = "conflict"
                page.evaluate("window.fixtureHold = true")
                save.click()
                page.wait_for_function("typeof window.fixtureRelease === 'function'")
                for control in (path, approval, save):
                    expect(control).to_be_disabled()
                page.evaluate("window.fixtureRelease()")
                expect(dialog.get_by_text("合成安装正在处理中", exact=False)).to_be_visible()
                expect(path).to_have_value(r"E:\Synthetic\OBS.zip")
                mode = "forbidden"
                page.locator("#tool-dialog-refresh").click()
                expect(approval).to_be_enabled()
                approval.check()
                save.click()
                expect(page.locator("#home-view")).to_be_visible()
                expect(path).to_have_value(r"E:\Synthetic\OBS.zip")
                mode = "bad_get"
                page.locator("#tool-dialog-refresh").click()
                expect(dialog.get_by_text("专用输出安装状态响应无效，请刷新后重试。", exact=True)).to_be_visible()
                expect(path).to_have_value(r"E:\Synthetic\OBS.zip")
                mode = "late401"
                page.evaluate("window.fixtureHold = true")
                page.locator("#tool-dialog-refresh").click()
                page.wait_for_function("typeof window.fixtureRelease === 'function'")
                page.locator("#tool-dialog-close").click()
                page.get_by_role("button", name="管理音频处理", exact=True).click()
                before = dialog.inner_html()
                page.evaluate("window.fixtureRelease()")
                page.wait_for_timeout(100)
                expect(page.locator("#home-view")).to_be_visible()
                assert dialog.inner_html() == before
                page.locator("#tool-dialog-close").click()
                mode = "normal"
                page.get_by_role("button", name="管理OBS 节目预览", exact=True).click()
                expect(path).to_have_value(r"E:\Synthetic\OBS.zip")
                approval.check()
                page.evaluate("window.fixtureHold = true")
                save.click()
                page.wait_for_function("typeof window.fixtureRelease === 'function'")
                page.locator("#tool-dialog-close").click()
                page.get_by_role("button", name="管理OBS 节目预览", exact=True).click()
                page.evaluate("window.fixtureRelease()")
                expect(save).to_be_disabled()
                page.locator("#tool-dialog-refresh").click()
                expect(dialog.get_by_text("OBS 33.0.1 已安装 · 重启应用后启用专用输出。", exact=True)).to_be_visible()
                expect(save).to_be_hidden()
                assert len(writes) == 3 and not errors and not unexpected, (writes, errors, unexpected)
                browser.close()
        finally:
            stop(process)
    print("OBS setup UI replay passed; synthetic routes only.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    arguments = parser.parse_args()
    verify(arguments.binary.resolve(), arguments.output.resolve())
