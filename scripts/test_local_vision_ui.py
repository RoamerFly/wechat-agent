"""Synthetic local visual-service settings acceptance; never calls a model."""

import argparse
import json
from pathlib import Path
import re
import tempfile

from playwright.sync_api import expect, sync_playwright

from test_local_home_ui import assert_no_horizontal_overflow, fulfill_json, home_tab, open_project_tool, start, stop


def verify(binary, output):
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="synthetic-local-vision-", dir=output) as directory:
        root = Path(directory)
        process, origin = start(binary, root / "host", root / "workspace")
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(channel="msedge", headless=True)
                for width, color, suffix in ((1280, "light", "desktop-light"), (375, "dark", "mobile-dark")):
                    context = browser.new_context(viewport={"width": width, "height": 800}, color_scheme=color)
                    page = context.new_page()
                    installed = False
                    gets = 0
                    puts = []
                    conflict = False
                    late_401 = False
                    view = {"revision": 1, "configuration": {"endpoint": "https://example.com/v1/chat/completions", "model": "synthetic-vision", "backend": "openai-compatible", "max_tokens": 1024, "thinking": False, "reasoning_effort": "low"}, "key_configured": True, "ready": True, "reason": ""}
                    page.add_init_script("""(() => {
                        window.fixtureDelayVision = false;
                        const nativeFetch = window.fetch.bind(window);
                        window.fetch = (...args) => {
                            const target = typeof args[0] === 'string' ? args[0] : args[0]?.url || '';
                            const result = nativeFetch(...args);
                            return String(target).endsWith('/media-understanding/vision') && window.fixtureDelayVision
                              ? result.then(value => new Promise(resolve => setTimeout(() => resolve(value), 800))) : result;
                        };
                    })();""")

                    def tools(route):
                        assert route.request.method == "GET", "视觉设置不得修改ASR工具启用状态"
                        fulfill_json({"tools": [{"id": "media-understanding", "name": "本机媒体理解", "installed": installed, "enabled": True, "runtime_ready": True, "catalog_available": True, "revision": 7}]})(route)

                    def vision(route):
                        nonlocal gets, view, conflict, late_401
                        if route.request.method == "GET":
                            gets += 1
                            if late_401:
                                late_401 = False
                                route.fulfill(status=401, content_type="application/json", body='{"error":"synthetic late session response"}')
                            else:
                                fulfill_json(view)(route)
                            return
                        assert route.request.method == "PUT"
                        body = route.request.post_data_json
                        puts.append(body)
                        assert set(body) == {"revision", "configuration", "api_key", "clear_key", "approve_model"}
                        assert body["approve_model"] is True
                        if conflict:
                            conflict = False
                            view = {**view, "revision": view["revision"] + 1}
                            route.fulfill(status=409, content_type="application/json", body='{"error":"视觉或语音识别配置已更新。当前草稿保留，请点击顶部刷新后核对再保存。"}')
                            return
                        assert body["revision"] == view["revision"]
                        view = {**view, "revision": view["revision"] + 1, "configuration": body["configuration"], "key_configured": False if body["clear_key"] else bool(body["api_key"]) or view["key_configured"]}
                        view["ready"] = view["key_configured"]
                        fulfill_json(view)(route)

                    page.route(re.compile(r".*/api/local/project-tools$"), tools)
                    page.route("**/api/local/project-tools/media-understanding/vision", vision)
                    page.goto(origin)
                    page.locator("#login-password").fill("synthetic-home-owner")
                    page.locator("#login-submit").click()
                    page.locator("#home-view").wait_for(state="visible")
                    if page.locator("#initialize-button").is_visible(timeout=500):
                        page.locator("#initialize-button").click()
                        page.locator("#initialize-button").wait_for(state="hidden")
                    home_tab(page, "project-tools")
                    page.locator("#project-tools-list[aria-busy='false']").wait_for()
                    assert gets == 0, "工具列表不应预读视觉配置"
                    open_project_tool(page, "本机媒体理解")
                    assert page.locator(".local-vision-settings").count() == 0 and gets == 0, "未安装时不显示空表单"
                    page.locator("#tool-dialog-close").click()
                    installed = True
                    page.locator("#project-tools-refresh").click()
                    page.locator("#project-tools-list[aria-busy='false']").wait_for()
                    open_project_tool(page, "本机媒体理解")
                    page.get_by_label("视觉模型", exact=True).wait_for()
                    assert gets == 1
                    assert page.locator("#local-vision-key").input_value() == "", "已存密钥不得回显"
                    save = page.get_by_role("button", name="保存视觉设置", exact=True)
                    assert save.is_disabled()
                    assert save.locator("xpath=ancestor::header").count() == 1, "保存按钮应在顶部"
                    model = page.get_by_label("视觉模型", exact=True)
                    model.fill("synthetic-edited-vision")
                    page.get_by_label("接口类型", exact=True).select_option("deepseek")
                    page.get_by_label("开启思考", exact=True).check()
                    page.get_by_label("思考强度", exact=True).select_option("high")
                    page.get_by_label("输出上限", exact=True).fill("2048")
                    page.get_by_label("允许使用此视觉服务", exact=True).check()
                    page.locator("#tool-dialog-refresh").click()
                    page.locator(".local-vision-form input:not(:disabled)").first.wait_for()
                    expect(model).to_have_value("synthetic-edited-vision")
                    conflict = True
                    save.click()
                    page.get_by_text("视觉或语音识别配置已更新。当前草稿保留，请点击顶部刷新后核对再保存。", exact=True).wait_for()
                    expect(model).to_have_value("synthetic-edited-vision")
                    assert_no_horizontal_overflow(page, f"{width}-pixel local vision conflict")
                    page.locator("#tool-dialog").screenshot(path=str(output / f"local-vision-conflict-{suffix}.png"))
                    assert puts[-1]["api_key"] == "" and puts[-1]["clear_key"] is False
                    page.locator("#tool-dialog-refresh").click()
                    page.locator(".local-vision-form input:not(:disabled)").first.wait_for()
                    expect(model).to_have_value("synthetic-edited-vision")
                    save.click()
                    page.get_by_text("视觉设置已保存，未调用模型；语音识别配置保持不变。", exact=True).wait_for()
                    expect(save).to_be_disabled()
                    assert puts[-1]["configuration"]["thinking"] is True and puts[-1]["configuration"]["reasoning_effort"] == "high"
                    assert view["key_configured"] is True
                    assert save.is_disabled(), "每次保存后重新取得明确许可"
                    page.locator("#local-vision-key").fill("synthetic-not-a-real-key")
                    page.get_by_label("允许使用此视觉服务", exact=True).check()
                    save.click()
                    page.get_by_text("视觉设置已保存，未调用模型；语音识别配置保持不变。", exact=True).wait_for()
                    expect(save).to_be_disabled()
                    expect(page.locator("#local-vision-key")).to_have_value("")
                    assert puts[-1]["api_key"] == "synthetic-not-a-real-key"
                    page.get_by_label("清除已保存密钥", exact=True).check()
                    page.get_by_label("允许使用此视觉服务", exact=True).check()
                    save.click()
                    page.get_by_text("视觉设置已保存，未调用模型；语音识别配置保持不变。", exact=True).wait_for()
                    expect(save).to_be_disabled()
                    assert puts[-1]["clear_key"] is True and puts[-1]["api_key"] == "" and view["key_configured"] is False
                    assert_no_horizontal_overflow(page, f"{width}-pixel local vision settings")
                    assert page.locator("#tool-dialog").evaluate("node => node.scrollTop") == 0
                    page.locator("#tool-dialog").screenshot(path=str(output / f"local-vision-{suffix}.png"))
                    page.locator("#tool-dialog-close").click()
                    late_401 = True
                    page.evaluate("window.fixtureDelayVision = true")
                    open_project_tool(page, "本机媒体理解")
                    page.locator("#tool-dialog-close").click()
                    page.evaluate("window.fixtureDelayVision = false")
                    open_project_tool(page, "本机媒体理解")
                    page.wait_for_timeout(900)
                    assert page.locator("#home-view").is_visible(), "关闭详情的迟到401不能退出新会话"
                    assert page.locator("#local-vision-key").input_value() == ""
                    assert len(puts) == 4
                    page.locator("#tool-dialog-close").click()
                    page.locator("#local-logout-button").click()
                    page.locator("#login-view").wait_for(state="visible")
                    context.close()
                browser.close()
        finally:
            stop(process)
    print("Local visual-service UI replay passed (synthetic APIs only; no model or platform calls).")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    verify(args.binary.resolve(), args.output.resolve())
