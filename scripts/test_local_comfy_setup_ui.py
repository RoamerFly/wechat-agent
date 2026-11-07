"""Synthetic ComfyUI/YuE deployment UI replay; no real tool or model calls."""

import argparse
import json
from pathlib import Path
import tempfile
from urllib.parse import urlparse

from playwright.sync_api import expect, sync_playwright

from test_local_home_ui import assert_no_horizontal_overflow, start, stop


def fulfill(route, payload, status=200):
    route.fulfill(status=status, content_type="application/json", body=json.dumps(payload, ensure_ascii=False))


def deployment_tool(tool_id, server):
    return {"id": tool_id, "name": "YuE 本机作曲" if tool_id == "yue" else "ComfyUI 图像工作流",
            "catalog_available": True, "runtime_ready": server["configured"], "installed": False,
            "enabled": False, "revision": 1, "state": "idle", "workflows": [],
            "reason": "" if server["configured"] else f"{tool_id}_backend_not_configured"}


def verify(binary, output, tool_id="comfyui"):
    output.mkdir(parents=True, exist_ok=True)
    prefix, name = ("yue", "YuE") if tool_id == "yue" else ("comfy", "ComfyUI")
    other_id = "comfyui" if tool_id == "yue" else "yue"
    endpoint = f"/api/local/project-tools/{tool_id}/deployment"
    with tempfile.TemporaryDirectory(prefix=f"synthetic-{prefix}-setup-ui-", dir=output) as directory:
        root = Path(directory).resolve()
        process, origin = start(binary, root / "host", root / "workspace")
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(channel="msedge", headless=True)
                context = browser.new_context(viewport={"width": 1120, "height": 820}, color_scheme="light")
                page = context.new_page()
                page.add_init_script("""(() => {
                    const fetch = window.fetch.bind(window);
                    window.fixtureHold = false;
                    window.fetch = (...args) => {
                        const url = typeof args[0] === 'string' ? args[0] : args[0]?.url || '';
                        const response = fetch(...args);
                        if (!window.fixtureHold || !url.endsWith('/deployment')) return response;
                        window.fixtureHold = false; window.fixtureRelease = null;
                        return response.then(value => new Promise(resolve => {
                            window.fixtureRelease = () => resolve(value);
                        }));
                    };
                })();""")
                server = {"revision": "unconfigured", "configured": False, "read_only": False, "configuration": None}
                writes, errors, unexpected = [], [], []
                mode = "normal"
                page.on("pageerror", lambda error: errors.append(str(error)))

                def intercept(route):
                    nonlocal server
                    request, path = route.request, urlparse(route.request.url).path
                    if not request.url.startswith(origin + "/"):
                        unexpected.append(request.url)
                        route.abort()
                    elif path == "/api/local/project-tools":
                        assert request.method == "GET", "save must not install, enable or start a tool"
                        fulfill(route, {"tools": [deployment_tool(tool_id, server), deployment_tool(other_id, server)]})
                    elif path == endpoint:
                        if request.method == "GET":
                            if mode == "late401":
                                fulfill(route, {"error": "合成迟到登录失效"}, 401)
                            else:
                                fulfill(route, {"revision": "invalid"} if mode == "bad_get" else server)
                            return
                        assert request.method == "PUT" and request.headers.get("x-csrf-token")
                        body = request.post_data_json
                        writes.append(body)
                        assert body["approve_execution"] is True and body["revision"] == server["revision"]
                        configuration = body["configuration"]
                        assert configuration["schema_version"] == 1 and configuration["enabled"] is True
                        assert configuration["runtime"] == rf"E:\Tools\{name}"
                        assert configuration["models"].startswith(rf"E:\Models\{name}")
                        assert configuration["workflows"] == rf"E:\Workflows\{name}"
                        assert configuration["idle_seconds"] == 0
                        assert configuration["port"] == (8191 if tool_id == "yue" else 8190)
                        if tool_id == "yue":
                            assert configuration["ffmpeg"] == r"E:\Tools\FFmpeg\ffmpeg.exe"
                            assert configuration["manifest"] == ""
                            assert "workflow_pins" not in configuration and "ffmpeg_sha256" not in configuration
                        else:
                            assert configuration["manifest"] == "approved-manifest.json"
                        if mode == "conflict":
                            server = {**server, "revision": "external-revision"}
                            fulfill(route, {"error": "部署登记已更新"}, 409)
                        elif mode == "forbidden":
                            fulfill(route, {"error": "合成部署许可已撤回"}, 403)
                        elif mode == "bad_put":
                            fulfill(route, {"revision": "invalid"})
                        else:
                            server = {**server, "revision": f"saved-{len(writes)}", "configured": True,
                                      "configuration": configuration}
                            fulfill(route, server)
                    elif path.startswith("/api/local/project-tools/"):
                        assert path == f"/api/local/project-tools/{other_id}/deployment" and request.method == "GET", path
                        fulfill(route, {"revision": "other", "configured": False, "read_only": False, "configuration": None})
                    else:
                        route.continue_()

                page.route("**/*", intercept)
                page.goto(origin)
                page.locator("#login-password").fill("synthetic-home-owner")
                page.locator("#login-submit").click()
                page.locator("#initialize-button").click()
                page.locator("#project-tools-tab").click()
                page.get_by_role("button", name=f"管理{deployment_tool(tool_id, server)['name']}", exact=True).click()
                dialog, save = page.locator("#tool-dialog"), page.locator("#local-deployment-save")
                approval = dialog.get_by_label("确认这些运行文件和工作流已审核")
                runtime, models = (dialog.locator(f"#{prefix}-deployment-{key}") for key in ("runtime", "models"))
                expect(runtime).to_be_visible()
                for key, value in (("runtime", rf"E:\Tools\{name}"), ("models", rf"E:\Models\{name}"),
                                   ("workflows", rf"E:\Workflows\{name}")):
                    dialog.locator(f"#{prefix}-deployment-{key}").fill(value)
                if tool_id == "yue":
                    dialog.locator("#yue-deployment-ffmpeg").fill(r"E:\Tools\FFmpeg\ffmpeg.exe")
                else:
                    dialog.locator("#comfy-deployment-manifest").fill("approved-manifest.json")
                dialog.get_by_text("运行细节", exact=True).click()
                idle = dialog.locator(f"#{prefix}-deployment-idle_seconds")
                idle.fill("0")
                expect(dialog.get_by_text(f"闲置停止秒数填 0 时，沿用默认 {20 if tool_id == 'yue' else 5} 分钟。", exact=True)).to_be_visible()
                runtime.press("Enter")
                expect(save).to_be_disabled()
                assert not writes, "Enter without approval must not submit"
                approval.check()
                idle.fill("1")
                expect(approval).not_to_be_checked()
                expect(save).to_be_disabled()
                idle.fill("0")
                dialog.get_by_role("button", name="重新读取", exact=True).click()
                expect(dialog.get_by_text("已读取最新登记，当前编辑已保留", exact=False)).to_be_visible()
                expect(runtime).to_have_value(rf"E:\Tools\{name}")
                approval.check()
                page.evaluate("window.fixtureHold = true")
                save.click()
                page.wait_for_function("typeof window.fixtureRelease === 'function'")
                for control in (save, runtime, approval):
                    expect(control).to_be_disabled()
                page.evaluate("window.fixtureRelease()")
                expect(dialog.get_by_text("部署登记已保存；工具仍需单独启用。", exact=True)).to_be_visible()
                assert len(writes) == 1
                page.locator("#project-tools-list[aria-busy='false']").wait_for()
                dialog.screenshot(path=str(output / f"{prefix}-deployment-desktop-light.png"))
                page.set_viewport_size({"width": 375, "height": 820})
                page.emulate_media(color_scheme="dark")
                dialog.locator(".comfy-deployment-actions").scroll_into_view_if_needed()
                assert_no_horizontal_overflow(page, f"{name} deployment 375 dark")
                dialog.screenshot(path=str(output / f"{prefix}-deployment-narrow-dark.png"))
                box = approval.bounding_box()
                assert box and 14 <= box["width"] <= 20 and 14 <= box["height"] <= 20
                mode, draft = "conflict", rf"E:\Models\{name}-draft"
                models.fill(draft)
                approval.check()
                save.click()
                expect(dialog.get_by_text("部署登记已更新", exact=False)).to_be_visible()
                expect(models).to_have_value(draft)
                expect(approval).not_to_be_checked()
                mode = "normal"
                dialog.get_by_role("button", name="重新读取", exact=True).click()
                expect(dialog.get_by_text("已读取最新登记，当前编辑已保留", exact=False)).to_be_visible()
                expect(models).to_have_value(draft)
                approval.check()
                save.click()
                expect(dialog.get_by_text("部署登记已保存；工具仍需单独启用。", exact=True)).to_be_visible()
                assert writes[-1]["revision"] == "external-revision"
                models.fill(draft + "-retained")
                approval.check()
                mode = "bad_put"
                save.click()
                expect(dialog.get_by_text("部署登记返回格式无效", exact=False)).to_be_visible()
                expect(models).to_have_value(draft + "-retained")
                mode = "forbidden"
                save.click()
                expect(dialog.get_by_text("合成部署许可已撤回", exact=True)).to_be_visible()
                expect(page.locator("#home-view")).to_be_visible()
                mode = "bad_get"
                dialog.get_by_role("button", name="重新读取", exact=True).click()
                expect(dialog.get_by_text("部署登记返回格式无效", exact=False)).to_be_visible()
                expect(models).to_have_value(draft + "-retained")
                mode = "normal"
                server = {**server, "read_only": True, "reason": "合成环境正在使用，请先停用工具。"}
                dialog.get_by_role("button", name="重新读取", exact=True).click()
                expect(dialog.get_by_text(server["reason"], exact=True)).to_be_visible()
                expect(save).to_be_disabled()
                mode = "late401"
                page.evaluate("window.fixtureHold = true")
                dialog.get_by_role("button", name="重新读取", exact=True).click()
                page.wait_for_function("typeof window.fixtureRelease === 'function'")
                page.locator("#tool-dialog-close").click()
                page.get_by_role("button", name=f"管理{deployment_tool(other_id, server)['name']}", exact=True).click()
                other_prefix = "comfy" if other_id == "comfyui" else "yue"
                expect(dialog.locator(f"#{other_prefix}-deployment-runtime")).to_be_visible()
                before = dialog.inner_html()
                page.evaluate("window.fixtureRelease()")
                page.wait_for_timeout(150)
                expect(page.locator("#home-view")).to_be_visible()
                assert dialog.inner_html() == before, "late response overwrote the next tool dialog"
                assert not errors and not unexpected, (errors, unexpected)
                browser.close()
        finally:
            stop(process)
    print(json.dumps({"status": "passed", "tool": tool_id, "synthetic": True,
                      "model_calls": 0, "tool_starts": 0, "writes": len(writes)}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--tool", choices=("comfyui", "yue"), default="comfyui")
    arguments = parser.parse_args()
    verify(arguments.binary.resolve(), arguments.output.resolve(), arguments.tool)
