"""Opt-in local home acceptance: synthetic data, real HTTP, zero model/WeChat calls."""

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import queue
import re
import shutil
import subprocess
import tempfile
import threading
from urllib.parse import parse_qs, unquote, urlencode, urlparse

from playwright.sync_api import expect, sync_playwright


def start(binary, data, workspace):
    environment = dict(os.environ, AHS_ADMIN_PASSWORD="synthetic-home-owner")
    process = subprocess.Popen(
        [str(binary), "--desktop", "-data", str(data), "-workspace", str(workspace), "-listen", "127.0.0.1:0"],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=environment,
        creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0,
    )
    lines = queue.Queue()
    threading.Thread(target=lambda: lines.put(process.stdout.readline()), daemon=True).start()
    try:
        first = lines.get(timeout=30).decode("utf-8")
        endpoint = re.search(r"127\.0\.0\.1:\d+", first)
        if endpoint is None:
            raise RuntimeError("synthetic station did not report a loopback address")
        return process, "http://" + endpoint.group()
    except BaseException:
        process.terminate()
        process.wait(timeout=15)
        raise


def stop(process):
    if process.poll() is None:
        try:
            process.stdin.write(b"stop\n")
            process.stdin.flush()
            assert process.wait(timeout=15) == 0, "desktop service did not stop gracefully"
        except BaseException:
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=15)
            raise
    process.stdin.close()
    process.stdout.close()
    process.stderr.close()


def open_project_tool(page, name):
    page.get_by_role("button", name=f"管理{name}", exact=True).click()


def close_application(page):
    if page.locator("#tool-dialog").evaluate("node => node.open"):
        page.locator("#tool-dialog-close").click()
    if page.locator("#app-dialog").evaluate("node => node.open"):
        page.locator("#app-dialog-close").click()


def home_tab(page, tab):
    close_application(page)
    page.locator(f"#{tab}-tab").click()


def open_application(page, name="微信", section="账号与使用"):
    if not page.locator("#app-dialog").evaluate("node => node.open"):
        page.get_by_role("button", name=f"管理{name}", exact=True).click()
    page.locator("#app-dialog-tabs").get_by_role("button", name=section, exact=True).click()


def refresh_application(page):
    node = "#app-dialog-refresh" if page.locator("#app-dialog").evaluate("node => node.open") else "#apps-refresh"
    page.locator(node).click()
    page.locator("#apps-list[aria-busy='false']").wait_for()
    open_application(page)


def assert_no_horizontal_overflow(page, scene):
    assert page.evaluate("document.documentElement.scrollWidth <= innerWidth"), f"{scene}: narrow overflow"


def assert_worker_controls_fit(page):
    clipped = page.evaluate("""() => {
        const panel = document.querySelector('.app-worker');
        const area = panel.getBoundingClientRect();
        return [...panel.querySelectorAll('button, textarea, input, .delivery-recall select')].filter((node) => {
            if (!node.getClientRects().length || node.closest('details:not([open])')) return false;
            const bounds = node.getBoundingClientRect();
            return bounds.left < area.left - 1 || bounds.right > area.right + 1;
        }).map((node) => node.getAttribute('aria-label') || node.textContent.trim());
    }""")
    assert not clipped, f"visible worker controls clipped inside their card: {clipped}"


def app_connection_payload():
    """Synthetic registered application data for browser-only app status checks."""
    return {
        "items": [{"id": "wechat", "name": "微信", "registration": "station-native", "account_setup": True}],
    }


def wechat_connection_payload():
    return {
        "items": [{"id": "synthetic-wechat", "label": "合成微信账号", "primary": True}],
    }


def fulfill_json(payload, status=200):
    def handler(route):
        route.fulfill(status=status, content_type="application/json", body=json.dumps(payload, ensure_ascii=False))

    return handler


def project_tool_payload(installed=False, enabled=False, revision=1, state="idle", pdf_ready=False, update_available=False):
    return {"tools": [{
        "id": "document-workshop", "name": "文档工坊", "catalog_available": True,
        "runtime_ready": True, "installed": installed, "enabled": enabled,
        "revision": revision, "state": state, "pdf_ready": pdf_ready,
        "update_available": update_available,
    }]}


def assert_station_without_device_controls(context, page, origin):
    project_tools = context.request.get(origin + "/api/local/project-tools")
    assert project_tools.status == 200
    assert "mobile-control" not in {item["id"] for item in project_tools.json()["tools"]}
    assert context.request.get(origin + "/api/mobile").status == 404
    assert context.request.get(origin + "/api/plugin-packages/mobile-control").status == 404
    assert page.get_by_text("手机探索", exact=True).count() == 0
    assert page.get_by_text("设备控制", exact=True).count() == 0


def verify_missing_source(binary, output):
    """A copied executable has no sibling connected-apps directory, by design."""
    with tempfile.TemporaryDirectory(prefix="synthetic-bare-station-", dir=output) as directory:
        directory = Path(directory).resolve()
        bare = directory / "bare" / binary.name
        bare.parent.mkdir()
        shutil.copy2(binary, bare)
        process, origin = start(bare, directory / "host", directory / "workspace")
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(channel="msedge", headless=True)
                context = browser.new_context(viewport={"width": 1280, "height": 800}, color_scheme="light")
                page = context.new_page()
                page.goto(origin)
                page.locator("#login-password").fill("synthetic-home-owner")
                page.locator("#login-submit").click()
                page.locator("#initialize-button").click()
                page.locator("#chat-view").wait_for(state="visible")
                assert_station_without_device_controls(context, page, origin)
                close_application(page)
                page.locator("#chat-tab").focus()
                page.keyboard.press("ArrowRight")
                page.locator("#apps-view").wait_for(state="visible")
                assert page.evaluate("document.activeElement.id") == "apps-tab", "tab keyboard navigation"
                page.locator("#apps-list[aria-busy='false']").wait_for()
                page.get_by_text("暂无可用版本", exact=True).wait_for()
                assert page.get_by_role("button", name="安装接入版本").count() == 0
                page.screenshot(path=str(output / "apps-no-source-light.png"), full_page=True)
                browser.close()
        finally:
            stop(process)


def verify(binary, output):
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="synthetic-home-", dir=output) as directory:
        directory = Path(directory).resolve()
        data, workspace = directory / "host", directory / "workspace"
        process, origin = start(binary, data, workspace)
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(channel="msedge", headless=True)
                context = browser.new_context(viewport={"width": 1280, "height": 800}, color_scheme="light")
                page = context.new_page()
                errors = []
                page.on("pageerror", lambda error: errors.append(str(error)))
                page.goto(origin)
                assert context.request.get(origin + "/api/local/runtime").status == 401
                page.locator("#login-view").wait_for(state="visible")
                page.screenshot(path=str(output / "login-light.png"))
                page.locator("#login-password").fill("synthetic-home-owner")
                page.locator("#login-submit").click()
                page.locator("#initialize-button").wait_for(state="visible")
                assert page.locator("#login-view").is_hidden(), "login card remained visible after login"
                assert not (data / "fish-identity.json").exists(), "read created identity"
                runtime = context.request.get(origin + "/api/local/runtime").json()
                assert runtime["kind"] == "local-station" and runtime["pid"] == process.pid
                assert Path(runtime["data_directory"]).resolve() == data.resolve()
                page.locator("#initialize-button").click()
                page.locator("#chat-view").wait_for(state="visible")
                assert_station_without_device_controls(context, page, origin)
                initial = context.request.get(origin + "/api/local/home").json()
                assert initial["initialized"] and initial["home_access"]
                assert initial.get("chat_available") is True
                model = context.request.get(origin + "/api/local/model")
                assert model.status == 200
                model = model.json()
                assert model["provider"] == "deepseek"
                assert model["model"] == "deepseek-flash"
                assert model["thinking_enabled"] is True
                assert model["reasoning_effort"] == "low"
                assert model["key_configured"] is False
                page.locator("#chat-view").wait_for(state="visible")
                page.get_by_text("先设置聊天模型", exact=True).wait_for()
                assert page.locator("#chat-input").is_enabled(), "chat must remain available independently of workspace access"
                home_tab(page, "settings")
                page.locator("#settings-view").wait_for(state="visible")
                assert page.locator("#model-save").is_visible()
                page.locator("#model-name").fill("synthetic-unsaved-draft")
                assert page.get_by_text("测试模型连接", exact=True).count() == 0
                home_tab(page, "chat")
                home = Path(initial["directory"])
                assert home.is_relative_to(workspace)
                assert not (data / "accounts.json").exists()
                assert not (data / "companion").exists()
                assert context.request.get(origin + "/api/accounts").status == 404
                for name in ["Connected Apps", "Project Tools", "diary", "projects", "works", "scratch"]:
                    assert (home / name).is_dir(), name
                note = home / "diary" / "synthetic.md"
                note.write_text("a persistent companion home", encoding="utf-8")

                # The project-tool endpoint is a browser-only route fixture. It
                # validates explicit artifact approval and exercises install,
                # enable, disable, and an artifact-preserving update without
                # starting a real tool process.
                tool_state = {"installed": False, "enabled": False, "revision": 1, "state": "idle", "pdf_ready": False, "update_available": False}
                project_tools_pattern = re.compile(r".*/api/local/project-tools(?:/document-workshop)?$")

                def project_tools_route(route):
                    request = route.request
                    if request.method == "GET":
                        route.fulfill(content_type="application/json", body=json.dumps(project_tool_payload(**tool_state), ensure_ascii=False))
                        return
                    assert request.method == "PUT"
                    body = request.post_data_json
                    assert body["revision"] == tool_state["revision"]
                    if body["action"] == "install":
                        assert body["approve_artifacts"] is True
                        tool_state.update(installed=True, enabled=False, revision=2)
                    elif body["action"] == "enable":
                        assert body["approve_artifacts"] is False
                        tool_state.update(enabled=True, revision=3, state="idle")
                    elif body["action"] == "disable":
                        assert body["approve_artifacts"] is False
                        tool_state.update(enabled=False, revision=4, state="idle", update_available=True)
                    elif body["action"] == "upgrade":
                        assert tool_state["enabled"] is False
                        assert body["approve_artifacts"] is True
                        tool_state.update(revision=5, state="idle", pdf_ready=True, update_available=False)
                    else:
                        raise AssertionError("unexpected project-tool action")
                    route.fulfill(content_type="application/json", body=json.dumps({"tool": project_tool_payload(**tool_state)["tools"][0]}, ensure_ascii=False))

                page.route(project_tools_pattern, project_tools_route)
                home_tab(page, "project-tools")
                page.locator("#project-tools-view").wait_for(state="visible")
                open_project_tool(page, "文档工坊")
                install = page.get_by_role("button", name="安装", exact=True)
                assert install.is_disabled(), "artifact confirmation must gate installation"
                page.get_by_role("checkbox", name="确认本机素材读写", exact=True).check()
                install.click()
                page.get_by_text("文档工坊已安装。", exact=True).wait_for()
                page.get_by_role("button", name="启用", exact=True).click()
                page.get_by_text("文档工坊已启用。", exact=True).wait_for()
                page.get_by_text("已启用，待命", exact=True).wait_for()
                page.get_by_role("button", name="停用", exact=True).click()
                page.get_by_text("文档工坊已停用。", exact=True).wait_for()
                page.get_by_text("已安装，可更新", exact=True).wait_for()
                upgrade = page.get_by_role("button", name="更新", exact=True)
                assert upgrade.is_disabled(), "artifact confirmation must gate project-tool update"
                page.get_by_role("checkbox", name="确认本机素材读写", exact=True).check()
                upgrade.click()
                page.get_by_text("文档工坊已更新。", exact=True).wait_for()
                page.get_by_text("PDF 导出环境已就绪。", exact=True).wait_for()
                page.screenshot(path=str(output / "project-tools-light.png"), full_page=True)
                page.unroute(project_tools_pattern, project_tools_route)

                audio_catalog = next(tool for tool in context.request.get(origin + "/api/local/project-tools").json()["tools"]
                                     if tool["id"] == "audio-workshop")
                if audio_catalog["catalog_available"] and audio_catalog["runtime_ready"]:
                    home_tab(page, "chat")
                    home_tab(page, "project-tools")
                    open_project_tool(page, "音频工坊")
                    audio = page.locator("#tool-dialog .project-tool-record")
                    install = audio.get_by_role("button", name="安装", exact=True)
                    install.wait_for()
                    assert install.is_disabled()
                    audio.get_by_role("checkbox", name="确认本机素材读写").check()
                    install.click()
                    page.get_by_text("音频工坊已安装。", exact=True).wait_for()
                    audio.get_by_role("button", name="启用", exact=True).click()
                    page.get_by_text("音频工坊已启用。", exact=True).wait_for()
                    page.screenshot(path=str(output / "project-tools-audio-light.png"), full_page=True)
                    audio.get_by_role("button", name="停用", exact=True).click()
                    page.get_by_text("音频工坊已停用。", exact=True).wait_for()

                rvc = next(tool for tool in context.request.get(origin + "/api/local/project-tools").json()["tools"]
                           if tool["id"] == "rvc")
                if rvc["catalog_available"]:
                    # Exercise the real install/enable endpoint. These are
                    # registration placeholders, and no conversion is started.
                    assert not rvc["runtime_ready"]
                    rvc_root = data.parent / "synthetic-rvc-resources"
                    media = data.parent / "synthetic-rvc-media"
                    for path in (rvc_root / "public_workspace", rvc_root / "pri_workspace", media):
                        path.mkdir(parents=True)
                    for executable in ("ffmpeg.exe", "ffprobe.exe"):
                        (media / executable).touch()
                    (data / "station-rvc.json").write_text(json.dumps({
                        "schema_version": 1, "enabled": True, "workspace": str(rvc_root),
                        "media": str(media), "port": 21981,
                        "profiles": [{"id": "private-synthetic", "label": "合成试听声线"}],
                    }, ensure_ascii=False), encoding="utf-8")
                    home_tab(page, "chat")
                    home_tab(page, "project-tools")
                    open_project_tool(page, "RVC 换声")
                    card = page.locator("#tool-dialog .project-tool-record")
                    card.get_by_text("合成试听声线", exact=True).wait_for()
                    install = card.get_by_role("button", name="安装", exact=True)
                    assert install.is_disabled()
                    card.get_by_role("checkbox", name="确认任务素材读写与本机 GPU").check()
                    install.click()
                    page.get_by_text("RVC 换声已安装。", exact=True).wait_for()
                    card.get_by_role("button", name="启用", exact=True).click()
                    page.get_by_text("RVC 换声已启用。", exact=True).wait_for()
                    card.get_by_text("已启用，等待任务", exact=True).wait_for()
                    page.screenshot(path=str(output / "project-tools-rvc-light.png"), full_page=True)
                    card.get_by_role("button", name="停用", exact=True).click()
                    page.get_by_text("RVC 换声已停用。", exact=True).wait_for()

                comfy = next(tool for tool in context.request.get(origin + "/api/local/project-tools").json()["tools"]
                             if tool["id"] == "comfyui")
                if comfy["catalog_available"]:
                    assert not comfy["runtime_ready"]
                    # Only metadata preflight runs in this browser replay.
                    runtime, models, workflows = (data.parent / f"synthetic-comfy-{key}" for key in ("runtime", "models", "workflows"))
                    for path in (runtime / "ComfyUI", runtime / "frontend", models, workflows):
                        path.mkdir(parents=True)
                    python, entry = Path("python.exe"), Path("ComfyUI") / "main.py"
                    pins = {}
                    for path in (python, entry):
                        payload = b"browser fixture, never launched"
                        (runtime / path).write_bytes(payload)
                        pins[str(path)] = hashlib.sha256(payload).hexdigest()
                    manifest = data / "comfyui-manifest.json"
                    manifest.write_text(json.dumps({
                        "schema_version": 1, "policy": "station-comfyui-v1", "files": pins,
                    }), encoding="utf-8")
                    (data / "station-comfyui.json").write_text(json.dumps({
                        "schema_version": 1, "enabled": True, "runtime": str(runtime), "python": str(python),
                        "entry": str(entry), "frontend": "frontend", "manifest": str(manifest),
                        "models": str(models), "workflows": str(workflows), "port": 21984, "device": "cpu",
                    }), encoding="utf-8")
                    home_tab(page, "chat")
                    home_tab(page, "project-tools")
                    open_project_tool(page, "ComfyUI 创作")
                    card = page.locator("#tool-dialog .project-tool-record")
                    card.get_by_text("工作流未就绪", exact=True).wait_for()
                    install = card.get_by_role("button", name="安装", exact=True)
                    assert install.is_disabled()
                    card.get_by_role("checkbox", name="确认任务成品写入与本机 GPU").check()
                    install.click()
                    page.get_by_text("ComfyUI 创作已安装。", exact=True).wait_for()
                    assert card.get_by_role("button", name="启用", exact=True).is_disabled()
                    graph = b'{"1":{"class_type":"EmptyImage","inputs":{"width":32,"height":24,"batch_size":1,"color":0}},"9":{"class_type":"SaveImage","inputs":{"images":["1",0]}}}'
                    (workflows / "smoke.json").write_bytes(graph)
                    (workflows / "registry.json").write_text(json.dumps({
                        "schema_version": 1, "comfyui_version": "0.37.2", "workflows": [{
                            "id": "smoke", "label": "离线图像样例", "graph": "smoke.json",
                            "sha256": hashlib.sha256(graph).hexdigest(), "save_node": "9", "parameters": [],
                        }],
                    }, ensure_ascii=False), encoding="utf-8")
                    page.locator("#project-tools-refresh").click()
                    card.get_by_text("离线图像样例", exact=True).wait_for()
                    card.get_by_role("button", name="启用", exact=True).click()
                    page.get_by_text("ComfyUI 创作已启用。", exact=True).wait_for()
                    card.get_by_text("已启用，等待任务", exact=True).wait_for()
                    page.screenshot(path=str(output / "project-tools-comfyui-light.png"), full_page=True)
                    card.get_by_role("button", name="停用", exact=True).click()
                    page.get_by_text("ComfyUI 创作已停用。", exact=True).wait_for()

                yue = next(tool for tool in context.request.get(origin + "/api/local/project-tools").json()["tools"]
                           if tool["id"] == "yue")
                if yue["catalog_available"]:
                    # YuE packages may be installed before a private official
                    # backend is registered. This browser replay deliberately
                    # does not manufacture a backend path or activate it.
                    assert not yue["runtime_ready"]
                    home_tab(page, "chat")
                    home_tab(page, "project-tools")
                    open_project_tool(page, "YuE 作曲")
                    card = page.locator("#tool-dialog .project-tool-record")
                    card.get_by_text("YuE 本机后端未就绪", exact=True).wait_for()
                    install = card.get_by_role("button", name="安装", exact=True)
                    assert install.is_disabled()
                    card.get_by_role("checkbox", name="确认任务素材读写与本机 GPU").check()
                    install.click()
                    page.get_by_text("YuE 作曲已安装。", exact=True).wait_for()
                    assert card.get_by_role("button", name="启用", exact=True).is_disabled()
                    page.screenshot(path=str(output / "project-tools-yue-light.png"), full_page=True)

                station_speech = next((tool for tool in context.request.get(origin + "/api/local/project-tools").json()["tools"]
                                       if tool["id"] == "station-speech"), None)
                if station_speech and station_speech["catalog_available"]:
                    # The official package can be installed before a private
                    # local backend and voice inventory are registered.
                    assert not station_speech["runtime_ready"]
                    home_tab(page, "chat")
                    home_tab(page, "project-tools")
                    open_project_tool(page, "本机语音")
                    card = page.locator("#tool-dialog .project-tool-record")
                    card.get_by_text("本机语音后端未就绪", exact=True).wait_for()
                    install = card.get_by_role("button", name="安装", exact=True)
                    assert install.is_disabled()
                    card.get_by_role("checkbox", name="确认本机语音成品写入与 GPU").check()
                    install.click()
                    page.get_by_text("本机语音已安装。", exact=True).wait_for()
                    assert card.get_by_role("button", name="启用", exact=True).is_disabled()
                    page.screenshot(path=str(output / "project-tools-station-speech-light.png"), full_page=True)

                # The catalog and source checkout use the real local HTTP and CSRF path.
                # A plain server executable may intentionally ship without application source;
                # in that case this primary replay still verifies the useful empty-source state.
                close_application(page)
                page.locator("#chat-tab").focus()
                page.keyboard.press("ArrowRight")
                page.locator("#apps-view").wait_for(state="visible")
                assert page.evaluate("document.activeElement.id") == "apps-tab", "tab keyboard navigation"
                page.locator("#apps-list[aria-busy='false']").wait_for()
                source_state = page.locator("#apps-list").inner_text()
                catalog = context.request.get(origin + "/api/local/apps/catalog").json()["applications"]
                assert len(catalog) == 1 and catalog[0]["id"] == "wechat"
                source_available = catalog[0]["source_available"]
                if source_available:
                    assert "有可用版本" in source_state, source_state
                    open_application(page, section="接入版本管理")
                    page.get_by_role("button", name="安装接入版本").click()
                    page.get_by_text("已安装", exact=True).wait_for()
                    page.locator("#apps-feedback").get_by_text(re.compile("接入版本已.*家目录")).wait_for()
                    assert (home / "Connected Apps" / "wechat" / "app.json").is_file(), "installed application integration version"
                else:
                    assert "暂无可用版本" in source_state, source_state
                    assert page.get_by_role("button", name="安装接入版本").count() == 0

                close_application(page)
                # Application-account HTTP calls are intentionally routed in the browser only:
                # no login material, native connector, or actual WeChat request is involved.
                apps_route = fulfill_json(app_connection_payload())
                connections_route = fulfill_json(wechat_connection_payload())
                page.route("**/api/apps", apps_route)
                page.route("**/api/apps/wechat/connections", connections_route)
                status_pattern = "**/api/apps/wechat/connections/synthetic-wechat"
                status_route = fulfill_json({"connection": {"id": "synthetic-wechat", "login": "authenticated"}})
                page.route(status_pattern, status_route)
                refresh_application(page)
                page.get_by_text("账号连接", exact=True).wait_for()
                page.get_by_text("合成微信账号", exact=True).wait_for()
                page.get_by_text("已登录", exact=True).wait_for()
                assert page.get_by_text("从旧版接入", exact=True).count() == 0, "legacy migration has no daily UI"
                page.screenshot(path=str(output / "apps-connected-light.png"), full_page=True)

                # Keep the same request key when the first response is lost,
                # then display the exact operation generation returned by status.
                connection_base = "**/api/apps/wechat/connections"
                login_pattern = status_pattern + "/login"
                operation_pattern = login_pattern + "/fixture-operation"
                qr_pattern = operation_pattern + "/qr?generation=2"
                requests = {"provision": [], "login": [], "qr": 0}
                progress = {"provisioned": False, "state": "awaiting_scan"}

                def setup_connections(route):
                    if route.request.method == "POST":
                        requests["provision"].append(route.request.post_data_json)
                        progress["provisioned"] = True
                        fulfill_json({"id": "synthetic-wechat", "account_id": "fixture"})(route)
                    else:
                        fulfill_json(wechat_connection_payload() if progress["provisioned"] else {"items": []})(route)

                def setup_start(route):
                    requests["login"].append(route.request.post_data_json)
                    if len(requests["login"]) == 1:
                        fulfill_json({"error": "暂时无法核对账号操作"}, status=503)(route)
                    else:
                        fulfill_json({"id": "fixture-operation", "state": "starting"})(route)

                def setup_operation(route):
                    state = progress["state"]
                    fulfill_json({"id": "fixture-operation", "state": state, "qr_available": state == "awaiting_scan", "qr_generation": 2})(route)

                def setup_qr(route):
                    requests["qr"] += 1
                    pixel = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII=")
                    route.fulfill(content_type="image/png", body=pixel)

                page.unroute(connection_base, connections_route)
                page.unroute(status_pattern, status_route)
                status_route = fulfill_json({"connection": {"id": "synthetic-wechat", "login": "signed_out", "runtime": "offline"}})
                page.route(connection_base, setup_connections)
                page.route(status_pattern, status_route)
                page.route(login_pattern, setup_start)
                page.route(operation_pattern, setup_operation)
                page.route(qr_pattern, setup_qr)
                refresh_application(page)
                page.get_by_text("账号未接入", exact=True).wait_for()
                page.get_by_role("button", name="添加账号").click()
                page.get_by_role("button", name="扫码登录").click()
                page.get_by_text("暂时无法核对账号操作", exact=True).wait_for()
                page.get_by_role("button", name="扫码登录").click()
                page.get_by_alt_text("微信登录二维码").wait_for()
                assert requests["provision"] and requests["login"][0] == requests["login"][1]
                page.wait_for_function("document.querySelector('.app-login img')?.naturalWidth > 0")
                assert requests["qr"] == 1
                page.screenshot(path=str(output / "apps-synthetic-qr-light.png"), full_page=True)
                progress["state"] = "expired"
                page.get_by_text("二维码已过期", exact=True).wait_for()
                assert page.get_by_alt_text("微信登录二维码").count() == 0
                page.unroute(login_pattern, setup_start)
                page.unroute(operation_pattern, setup_operation)
                page.unroute(qr_pattern, setup_qr)

                worker_policy_pattern = status_pattern + "/policy"
                worker_lifecycle_pattern = status_pattern + "/lifecycle"
                worker = {"state": "disabled", "threads": [], "history_threads": [], "attachment_threads": [], "media_reply_threads": [], "group_mute_threads": [], "group_rules": [], "contact_read": False, "auto_observe_images": False, "auto_observe_videos": False, "actions": [], "policy_payloads": []}
                legacy = {"availability": "available", "previews": 0, "posts": [], "source_removed": False}
                legacy_activation = {"gets": 0, "posts": []}
                legacy_rollback = {"gets": [], "posts": [], "audit_gets": 0, "audits": []}
                legacy_memory = {"requests": []}
                legacy_memory_selection = {"gets": [], "posts": []}
                legacy_group_rule = {"gets": [], "posts": []}
                legacy_receipts = {"pages": [], "lookups": [], "filters": [], "management_pages": [], "management_filters": []}

                def legacy_plan(applied=False):
                    refreshed = legacy["availability"] == "applied" and legacy["previews"] >= 3
                    cutover = ({"legacy_host": "changed", "native_profile": "pending", "target_login": "online",
                                "target_identity": "owner_check_pending"} if refreshed else
                               {"legacy_host": "stop_record_matched", "native_profile": "import_receipt_matched",
                                "target_login": "signed_out", "target_identity": "owner_check_pending"})
                    return {"digest": f"fixture-legacy-digest-{legacy['previews']}", "account_id": "fixture-account",
                            "suggested_reply_threads": ["synthetic-room@chatroom"],
                            "requires_manual_review": ["all_friends"],
                            "receipt_records": 3, "unresolved_receipts": 1,
                            "management_receipt_records": 2, "management_unresolved_receipts": 1,
                            "same_native_dataset": True, "legacy_enabled": True,
                            "legacy_paused": True, "applied": applied, "cutover": cutover}

                def worker_view():
                    connection = {"id": "synthetic-wechat", "login": "authenticated",
                        "runtime": "online", "worker": worker["state"], "configured_policy": {
                            "conversation_enabled": bool(worker["threads"]), "reply_threads": worker["threads"], "history_threads": worker["history_threads"],
                            "attachment_threads": worker["attachment_threads"], "media_reply_threads": worker["media_reply_threads"],
                            "group_mute_threads": worker["group_mute_threads"],
                            "group_rules": worker["group_rules"],
                            "contact_read": worker["contact_read"], "auto_observe_images": worker["auto_observe_images"], "auto_observe_videos": worker["auto_observe_videos"]}}
                    if legacy["availability"]:
                        connection["legacy_import"] = legacy["availability"]
                    return {"connection": connection}

                def read_worker(route):
                    fulfill_json(worker_view())(route)

                def save_worker(route):
                    assert route.request.method == "PUT"
                    body = route.request.post_data_json
                    assert set(body["media_reply_threads"]) <= set(body["attachment_threads"]), "media reply cannot survive attachment-read revocation"
                    worker["policy_payloads"].append(body)
                    worker["threads"] = body["reply_threads"]
                    worker["history_threads"] = body["history_threads"]
                    worker["attachment_threads"] = body["attachment_threads"]
                    worker["auto_observe_images"] = body["auto_observe_images"]
                    worker["auto_observe_videos"] = body["auto_observe_videos"]
                    worker["media_reply_threads"] = body["media_reply_threads"]
                    worker["group_mute_threads"] = body["group_mute_threads"]
                    worker["group_rules"] = [
                        {key: value for key, value in rule.items()
                         if key != "reply_settings" or value != {"mode": "mention"}}
                        for rule in body.get("group_rules", [])
                    ]
                    worker["contact_read"] = body["contact_read"]
                    fulfill_json(worker_view())(route)

                def set_worker(route):
                    assert route.request.method == "POST"
                    action = route.request.post_data_json["action"]
                    worker["actions"].append(action)
                    worker["state"] = "running" if action == "start" else "paused"
                    fulfill_json({"worker": worker["state"]})(route)

                page.unroute(status_pattern, status_route)
                status_route = read_worker
                page.route(status_pattern, status_route)
                page.route(worker_policy_pattern, save_worker)
                page.route(worker_lifecycle_pattern, set_worker)
                legacy_import_pattern = status_pattern + "/legacy-import"

                def legacy_import(route):
                    if route.request.method == "GET":
                        legacy["previews"] += 1
                        fulfill_json(legacy_plan(legacy["availability"] == "applied"))(route)
                        return
                    assert route.request.method == "POST"
                    legacy["posts"].append(route.request.post_data_json)
                    assert route.request.headers.get("x-csrf-token"), "legacy import lacks owner CSRF"
                    assert route.request.post_data_json == {"digest": f"fixture-legacy-digest-{legacy['previews']}"}
                    if len(legacy["posts"]) == 1:
                        fulfill_json({"error": "stale legacy preview"}, status=409)(route)
                        return
                    legacy["availability"] = "applied"
                    legacy["source_removed"] = True
                    fulfill_json(legacy_plan(True))(route)

                page.route(legacy_import_pattern, legacy_import)
                legacy_activation_pattern = legacy_import_pattern + "/activation"

                def legacy_activation_route(route):
                    if route.request.method == "GET":
                        legacy_activation["gets"] += 1
                        if legacy_activation["gets"] >= 3:
                            fulfill_json({"status": "blocked", "reason": "旧宿主停机记录已变化，请先重新导入后再启用",
                                          "legacy_host": "changed", "native_profile": "import_receipt_matched",
                                          "target_login": "online", "target_identity": "owner_check_pending",
                                          "reply_count": 1, "pending_receipts": 2, "management_pending_receipts": 1,
                                          "model_ready": True})(route)
                            return
                        digest = "fixture-activation-v2" if legacy_activation["gets"] >= 2 else "fixture-activation-v1"
                        fulfill_json({"status": "ready", "digest": digest, "legacy_host": "stop_record_matched",
                                      "native_profile": "import_receipt_matched", "target_login": "online",
                                      "target_identity": "owner_check_pending", "reply_count": 1,
                                      "pending_receipts": 2, "management_pending_receipts": 1, "model_ready": True})(route)
                        return
                    assert route.request.method == "POST"
                    assert route.request.headers.get("x-csrf-token"), "activation confirmation lacks owner CSRF"
                    payload = route.request.post_data_json
                    legacy_activation["posts"].append(payload)
                    assert payload["same_identity"] is True
                    assert set(payload) == {"digest", "same_identity", "reviewed_receipts"}
                    if len(legacy_activation["posts"]) == 1:
                        assert payload == {"digest": "fixture-activation-v1", "same_identity": True, "reviewed_receipts": True}
                        fulfill_json({"error": "stale activation check"}, status=409)(route)
                        return
                    assert payload == {"digest": "fixture-activation-v2", "same_identity": True, "reviewed_receipts": True}
                    fulfill_json({"status": "confirmed", "legacy_host": "stop_record_matched",
                                  "native_profile": "import_receipt_matched", "target_login": "online",
                                  "target_identity": "owner_confirmed", "reply_count": 1,
                                  "pending_receipts": 2, "management_pending_receipts": 1, "model_ready": True})(route)

                # Register after the import handler: Playwright resolves routes in reverse order.
                page.route(legacy_activation_pattern, legacy_activation_route)
                legacy_rollback_pattern = re.compile(r"/api/apps/wechat/connections/synthetic-wechat/legacy-import/rollback(?:\?.*)?$")
                rollback_cursor = "c" * 64 + ":rollback-unknown"

                def legacy_rollback_route(route):
                    if route.request.method == "POST":
                        assert route.request.headers.get("x-csrf-token"), "rollback freeze lacks owner CSRF"
                        assert route.request.post_data_json == {}
                        assert len(legacy_activation["posts"]) == 2, "freeze must remain unavailable before activation confirmation"
                        legacy_rollback["posts"].append({})
                        fulfill_json({"state": "draining", "eligible": True}, status=202)(route)
                        return
                    assert route.request.method == "GET", "rollback audit must only inspect receipts"
                    parsed = urlparse(route.request.url)
                    after = parse_qs(parsed.query, keep_blank_values=True).get("after", [""])[0]
                    legacy_rollback["gets"].append(after)
                    if not legacy_rollback["posts"]:
                        fulfill_json({"state": "not_frozen", "eligible": len(legacy_activation["posts"]) == 2,
                                      "items": []})(route)
                        return
                    if len(legacy_rollback["gets"]) == 2:
                        fulfill_json({"state": "draining", "eligible": True, "items": []})(route)
                        return
                    if after == rollback_cursor:
                        fulfill_json({"state": "frozen", "eligible": True, "frozen_at": "2026-11-07T08:00:00Z",
                                      "items": [{"idempotency_key": "rollback-terminal", "conversation_id": "synthetic-room@chatroom",
                                                 "kind": "voice", "state": "cancelled", "disposition": "terminal"}]})(route)
                        return
                    fulfill_json({"state": "frozen", "eligible": True, "frozen_at": "2026-11-07T08:00:00Z",
                                  "next_after": rollback_cursor, "items": [
                                      {"idempotency_key": "rollback-accepted", "conversation_id": "synthetic-room@chatroom",
                                       "kind": "text", "state": "accepted", "disposition": "accepted"},
                                      {"idempotency_key": "rollback-unknown", "conversation_id": "synthetic-room@chatroom",
                                       "kind": "file", "state": "submission_unknown", "disposition": "unknown"}
                                  ]})(route)

                page.route(legacy_rollback_pattern, legacy_rollback_route)
                legacy_rollback_audit_pattern = re.compile(r"/api/apps/wechat/connections/synthetic-wechat/legacy-import/rollback/audit$")

                def legacy_rollback_audit_route(route):
                    assert legacy_rollback["posts"], "audit must be unavailable before rollback freeze"
                    if route.request.method == "GET":
                        legacy_rollback["audit_gets"] += 1
                        if not legacy_rollback["audits"]:
                            fulfill_json({"error": "rollback audit not saved"}, status=404)(route)
                            return
                        fulfill_json(dict(legacy_rollback["audits"][-1], local_current=False))(route)
                        return
                    assert route.request.method == "POST"
                    assert route.request.headers.get("x-csrf-token"), "rollback audit save lacks owner CSRF"
                    assert route.request.post_data_json == {}
                    audit = {"digest": "fixture-rollback-audit-v1", "captured_at": "2026-11-07T08:02:00Z",
                             "delivery_count": 3, "action_count": 5, "active_count": 1, "unknown_count": 1,
                             "native_state": "available", "desktop_bound": True, "local_current": True}
                    legacy_rollback["audits"].append(audit)
                    fulfill_json(audit)(route)

                page.route(legacy_rollback_audit_pattern, legacy_rollback_audit_route)
                legacy_receipts_pattern = re.compile(r"/api/apps/wechat/connections/synthetic-wechat/legacy-import/receipts(?:/[^/?]+)?(?:\?.*)?$")
                long_legacy_key = "legacy-accepted-" + "a" * 110

                def legacy_receipt_records(route):
                    assert route.request.method == "GET", "legacy receipt inspection must stay read-only"
                    assert legacy["availability"] == "applied" and legacy["source_removed"], "legacy snapshot must remain readable after source removal"
                    parsed = urlparse(route.request.url)
                    path = parsed.path
                    if path.endswith("/legacy-import/receipts"):
                        query = parse_qs(parsed.query, keep_blank_values=True)
                        assert query.get("limit") == ["20"]
                        after = query.get("after", [""])[0]
                        pending_only = query.get("pending", [""])[0] == "1"
                        origin = query.get("origin", ["agent"])[0]
                        assert origin in {"agent", "management"}
                        if origin == "management":
                            legacy_receipts["management_pages"].append(after)
                            legacy_receipts["management_filters"].append(pending_only)
                            if pending_only:
                                fulfill_json({"items": [{"idempotency_key": "management-unknown", "conversation_id": "synthetic-room@chatroom",
                                                         "kind": "text", "state": "submission_unknown", "lookup": "legacy_management_snapshot"}],
                                              "coverage": "legacy_management_snapshot", "pending_count": 1, "receipt_count": 2})(route)
                                return
                            if after == "fixture-management-next":
                                fulfill_json({"items": [{"idempotency_key": "management-complete", "conversation_id": "synthetic-room@chatroom",
                                                         "kind": "file", "state": "accepted", "lookup": "legacy_management_snapshot"}],
                                              "coverage": "legacy_management_snapshot", "pending_count": 1, "receipt_count": 2})(route)
                                return
                            fulfill_json({"items": [
                                {"idempotency_key": "management-unknown", "conversation_id": "synthetic-room@chatroom",
                                 "kind": "text", "state": "submission_unknown", "lookup": "legacy_management_snapshot"},
                                {"idempotency_key": "management-accepted", "conversation_id": "synthetic-direct@chatroom",
                                 "kind": "image", "state": "accepted", "lookup": "legacy_management_snapshot"}
                            ], "next_after": "fixture-management-next", "coverage": "legacy_management_snapshot",
                              "pending_count": 1, "receipt_count": 2})(route)
                            return
                        legacy_receipts["pages"].append(after)
                        legacy_receipts["filters"].append(pending_only)
                        if pending_only:
                            fulfill_json({"items": [{"idempotency_key": "legacy-unknown", "conversation_id": "synthetic-room@chatroom",
                                                     "kind": "text", "state": "submission_unknown", "lookup": "legacy_snapshot",
                                                     "operation_id": "legacy-operation-unknown"}], "snapshot": "fixture-legacy-receipts-v1",
                                          "coverage": "legacy_snapshot", "pending_count": 2, "receipt_count": 4})(route)
                            return
                        if after == "fixture-legacy-stale":
                            fulfill_json({"error": "stale legacy receipt snapshot"}, status=409)(route)
                            return
                        if after == "fixture-legacy-next":
                            fulfill_json({"items": [{"idempotency_key": "legacy-complete", "conversation_id": "synthetic-room@chatroom",
                                                         "kind": "text", "state": "accepted", "lookup": "legacy_snapshot",
                                                         "server_id": "legacy-server-3"}], "snapshot": "fixture-legacy-receipts-v2",
                                          "coverage": "legacy_snapshot", "pending_count": 2, "receipt_count": 4})(route)
                            return
                        next_after = "fixture-legacy-stale" if legacy_receipts["pages"].count("") == 1 else "fixture-legacy-next"
                        fulfill_json({"items": [
                            {"idempotency_key": "legacy-pending", "conversation_id": "synthetic-room@chatroom",
                             "kind": "voice", "state": "submitted_pending", "lookup": "legacy_snapshot",
                             "operation_id": "legacy-operation-1", "accepted_parts": [{"index": 0, "server_id": "legacy-server-1"}],
                             "unconfirmed_parts": [{"index": 1, "state": "submitted_pending"}], "parts_incomplete": True},
                            {"idempotency_key": long_legacy_key, "conversation_id": "synthetic-direct@chatroom",
                             "kind": "file", "state": "accepted", "lookup": "legacy_snapshot", "server_id": "legacy-server-2"},
                            {"idempotency_key": "legacy-unknown", "conversation_id": "synthetic-room@chatroom",
                             "kind": "text", "state": "submission_unknown", "lookup": "legacy_snapshot",
                             "operation_id": "legacy-operation-unknown"}
                        ], "next_after": next_after, "snapshot": "fixture-legacy-receipts-v1",
                                      "coverage": "legacy_snapshot", "pending_count": 2, "receipt_count": 4})(route)
                        return
                    key = unquote(path.rsplit("/", 1)[-1])
                    legacy_receipts["lookups"].append(key)
                    assert key in {"legacy-pending", "legacy-unknown"}
                    if key == "legacy-unknown":
                        fulfill_json({"idempotency_key": "legacy-unknown", "conversation_id": "synthetic-room@chatroom",
                                      "kind": "text", "state": "submission_unknown", "lookup": "not_found",
                                      "operation_id": "legacy-operation-unknown"})(route)
                        return
                    fulfill_json({"idempotency_key": "legacy-pending", "conversation_id": "synthetic-room@chatroom",
                                  "kind": "voice", "state": "accepted", "lookup": "platform", "operation_id": "legacy-operation-1",
                                  "server_id": "legacy-server-4", "accepted_parts": [{"index": 0, "server_id": "legacy-server-1"},
                                                                                          {"index": 1, "server_id": "legacy-server-4"}]})(route)

                page.route(legacy_receipts_pattern, legacy_receipt_records)
                legacy_group_rule_pattern = legacy_import_pattern + "/groups/*/rule"

                def legacy_group_rule_import(route):
                    path = urlparse(route.request.url).path
                    thread = unquote(path.split("/groups/", 1)[1].rsplit("/rule", 1)[0])
                    assert thread == "synthetic-room@chatroom"
                    if route.request.method == "GET":
                        legacy_group_rule["gets"].append(thread)
                        fulfill_json({"status": "ready", "digest": "fixture-legacy-group-rule",
                                      "reply_settings": {"mode": "social", "prefix": "!fish ",
                                                         "persona": "旧版群角色 <原样显示>",
                                                         "force_reply_when_mentioned": True},
                                      "reason": "已找到旧版群触发方式"})(route)
                        return
                    assert route.request.method == "POST"
                    assert route.request.headers.get("x-csrf-token"), "group-rule import lacks owner CSRF"
                    legacy_group_rule["posts"].append(route.request.post_data_json)
                    assert legacy_group_rule["posts"] == [{"digest": "fixture-legacy-group-rule", "same_identity": True}]
                    worker["group_rules"] = [{"thread_id": "synthetic-room@chatroom", "note": "收到分享时先总结重点", "summarize_shares": True,
                                              "reply_settings": {"mode": "social", "prefix": "!fish ", "persona": "旧版群角色 <原样显示>",
                                                                 "force_reply_when_mentioned": True}}]
                    fulfill_json({"status": "applied", "reason": "已接续旧版群触发方式"})(route)

                page.route(legacy_group_rule_pattern, legacy_group_rule_import)
                legacy_memory_pattern = legacy_import_pattern + "/memory*"
                note_text = "合成旧群备注：<原样显示>"
                note_posts = []

                def legacy_memory_report(route):
                    path = urlparse(route.request.url).path
                    selected_prefix = "/memory/"
                    if selected_prefix in path:
                        thread = unquote(path.split(selected_prefix, 1)[1])
                        if thread == "synthetic-room@chatroom/preference":
                            if route.request.method == "GET":
                                fulfill_json({"status": "ready", "digest": "fixture-legacy-note", "note": note_text,
                                              "can_enable_channels_summary": True})(route)
                            else:
                                assert route.request.method == "POST"
                                assert route.request.headers.get("x-csrf-token")
                                note_posts.append(route.request.post_data_json)
                                assert note_posts == [{"digest": "fixture-legacy-note", "same_identity": True,
                                                       "enable_channels_summary": True}]
                                fulfill_json({"status": "applied"})(route)
                            return
                        assert thread == "synthetic-room@chatroom"
                        if route.request.method == "GET":
                            legacy_memory_selection["gets"].append(thread)
                            digest = "fixture-memory-selection-v2" if len(legacy_memory_selection["gets"]) > 1 else "fixture-memory-selection-v1"
                            fulfill_json({"status": "ready", "digest": digest, "event_count": 12,
                                          "memory_count": 3, "reason": "当前 Agent 已暂停，接续不会自动启动"})(route)
                            return
                        assert route.request.method == "POST"
                        assert route.request.headers.get("x-csrf-token"), "memory continuation lacks owner CSRF"
                        payload = route.request.post_data_json
                        legacy_memory_selection["posts"].append(payload)
                        assert payload.get("same_identity") is True
                        if len(legacy_memory_selection["posts"]) == 1:
                            assert payload == {"digest": "fixture-memory-selection-v1", "same_identity": True}
                            fulfill_json({"error": "stale memory preview"}, status=409)(route)
                            return
                        assert payload == {"digest": "fixture-memory-selection-v2", "same_identity": True}
                        fulfill_json({"status": "applied", "event_count": 12, "memory_count": 3,
                                      "reason": "已接续到当前账号的记忆库"})(route)
                        return
                    assert route.request.method == "GET"
                    query = parse_qs(urlparse(route.request.url).query, keep_blank_values=True)
                    after = query.get("after", [""])[0]
                    legacy_memory["requests"].append(after)
                    if after == "fixture-memory-next":
                        fulfill_json({"status": "ready", "matched_scopes": 2, "unmapped_scopes": 1, "foreign_scopes": 0,
                                      "conversations": [{"thread_id": "synthetic-room-older@chatroom", "event_count": 4,
                                                         "withheld_events": 1, "memory_count": 1, "has_summary": False,
                                                         "history_granted": False}]})(route)
                        return
                    if len([item for item in legacy_memory["requests"] if not item]) > 1 and legacy["availability"] != "applied":
                        fulfill_json({"status": "snapshot_incomplete", "matched_scopes": 2, "unmapped_scopes": 1,
                                      "foreign_scopes": 0, "conversations": []})(route)
                        return
                    fulfill_json({"status": "ready", "matched_scopes": 2, "unmapped_scopes": 1, "foreign_scopes": 0,
                                  "conversations": [{"thread_id": "synthetic-room@chatroom", "event_count": 12,
                                                     "withheld_events": 2, "memory_count": 3, "has_summary": True,
                                                     "history_granted": True, "has_preference": True}], "next_after": "fixture-memory-next"})(route)

                page.route(legacy_memory_pattern, legacy_memory_report)
                # Register the per-conversation route after the inventory route.
                # Playwright runs matching handlers in reverse registration order,
                # so this keeps a selected scope out of the paged inventory handler.
                page.route(legacy_import_pattern + "/memory/*", legacy_memory_report)
                page.route(legacy_import_pattern + "/memory/*/preference", legacy_memory_report)
                contacts_pattern = status_pattern + "/contacts*"
                contact_queries = []

                def contact_directory(route):
                    assert route.request.method == "GET"
                    assert "/synthetic-wechat/contacts" in route.request.url
                    query = parse_qs(urlparse(route.request.url).query, keep_blank_values=True)
                    contact_queries.append(query)
                    assert query["search"] == ["蓝"]
                    if query["limit"] == ["500"]:
                        assert "cursor" not in query
                        fulfill_json({"items": [{"id": "wxid-synthetic-4", "display_name": "大页联系人"}], "has_more": False,
                                      "effective_limit": 200, "limit_reason": "原生接口单页上限为 200 条，可用续读位置继续。",
                                      "coverage": "已读取当前连接的本机通讯录快照"})(route)
                        return
                    assert query["limit"] == ["50"]
                    if query.get("cursor") == ["synthetic-next"]:
                        fulfill_json({"items": [{"id": "wxid-synthetic-3", "display_name": "蓝鲸"}], "has_more": False, "effective_limit": 50,
                                      "coverage": "已读取 3 位匹配联系人"})(route)
                    else:
                        assert "cursor" not in query
                        fulfill_json({"items": [{"id": "wxid-synthetic-1", "display_name": "蓝色大肥鱼"},
                                                {"id": "wxid-synthetic-2", "display_name": "蓝朋友"}],
                                      "next_cursor": "synthetic-next", "has_more": True,
                                      "effective_limit": 50,
                                      "coverage": "已读取前 2 位匹配联系人"})(route)

                page.route(contacts_pattern, contact_directory)
                refresh_application(page)
                page.get_by_text("Agent 未启动", exact=True).wait_for()
                assert page.get_by_role("button", name="启动 Agent").is_disabled()
                page.get_by_role("button", name="设置回复范围").click()
                page.get_by_role("textbox", name="回复会话 ID", exact=True).fill("filehelper\nsynthetic-room@chatroom")
                page.get_by_role("textbox", name="历史与成员读取会话 ID", exact=True).fill("synthetic-room@chatroom")
                summary_share = page.get_by_role("checkbox", name="synthetic-room@chatroom 自动总结视频号分享", exact=True)
                assert summary_share.is_disabled(), "share summaries require a same-group attachment-read grant"
                page.get_by_role("textbox", name="附件读取会话 ID").fill("synthetic-room@chatroom")
                summary_share = page.get_by_role("checkbox", name="synthetic-room@chatroom 自动总结视频号分享", exact=True)
                assert summary_share.is_enabled(), "attachment-read grant should unlock only this group's summary switch"
                page.get_by_role("textbox", name="synthetic-room@chatroom 群聊备注", exact=True).fill("收到分享时先总结重点")
                refresh_application(page)
                page.locator("#apps-list[aria-busy='false']").wait_for()
                expect(page.get_by_role("textbox", name="synthetic-room@chatroom 群聊备注", exact=True)).to_have_value("收到分享时先总结重点")
                page.get_by_role("checkbox", name="synthetic-room@chatroom 自动总结视频号分享", exact=True).check()
                page.get_by_role("textbox", name="附件回复会话 ID").fill("synthetic-room@chatroom")
                page.get_by_role("textbox", name="Agent 免打扰群 ID", exact=True).fill("synthetic-mute@chatroom")
                page.get_by_role("checkbox", name="允许大肥鱼在本地聊天中查阅此通讯录").check()
                page.get_by_role("button", name="保存回复范围").click()
                page.locator("#apps-feedback").get_by_text("回复范围已保存。", exact=True).wait_for()
                page.get_by_role("button", name="启动 Agent").click()
                page.get_by_text("Agent 运行中", exact=True).wait_for()
                page.get_by_role("button", name="暂停 Agent").click()
                page.get_by_text("Agent 已暂停", exact=True).wait_for()
                assert worker["threads"] == ["filehelper", "synthetic-room@chatroom"]
                assert worker["history_threads"] == ["synthetic-room@chatroom"]
                assert page.get_by_role("textbox", name="历史与成员读取会话 ID", exact=True).input_value() == "synthetic-room@chatroom"
                assert worker["attachment_threads"] == ["synthetic-room@chatroom"]
                assert worker["group_rules"] == [{"thread_id": "synthetic-room@chatroom", "note": "收到分享时先总结重点", "summarize_shares": True}]
                assert worker["policy_payloads"][-1]["group_rules"] == worker["group_rules"], "group settings must use the local policy route only"
                page.locator("#apps-list[aria-busy='false']").wait_for()
                page.get_by_text("触发方式与角色（可选）", exact=True).click()
                assert page.get_by_role("combobox", name="synthetic-room@chatroom 群聊触发方式", exact=True).input_value() == "social"
                assert page.get_by_role("textbox", name="synthetic-room@chatroom 群聊触发前缀", exact=True).input_value() == "!fish "
                page.get_by_role("button", name="恢复默认触发方式", exact=True).click()
                expect(page.get_by_role("checkbox", name="自动读图", exact=True)).not_to_be_checked()
                expect(page.get_by_role("checkbox", name="自动读视频", exact=True)).not_to_be_checked()
                page.get_by_role("checkbox", name="自动读图", exact=True).check()
                page.get_by_role("checkbox", name="自动读视频", exact=True).check()
                page.get_by_role("button", name="保存回复范围").click()
                page.locator("#apps-feedback").get_by_text("回复范围已保存。", exact=True).wait_for()
                assert worker["policy_payloads"][-1]["group_rules"][0]["reply_settings"] == {"mode": "mention"}
                assert worker["group_rules"] == [{"thread_id": "synthetic-room@chatroom", "note": "收到分享时先总结重点", "summarize_shares": True}]
                assert page.get_by_role("textbox", name="附件读取会话 ID").input_value() == "synthetic-room@chatroom"
                assert worker["media_reply_threads"] == ["synthetic-room@chatroom"]
                assert page.get_by_role("textbox", name="附件回复会话 ID").input_value() == "synthetic-room@chatroom"
                assert worker["group_mute_threads"] == ["synthetic-mute@chatroom"]
                assert page.get_by_role("textbox", name="Agent 免打扰群 ID", exact=True).input_value() == "synthetic-mute@chatroom"
                assert worker["contact_read"] is True
                assert worker["auto_observe_images"] is True
                assert worker["auto_observe_videos"] is True
                assert worker["actions"] == ["start", "stop"]
                worker["state"] = "cutover_blocked"
                refresh_application(page)
                page.get_by_text("等待完成启用核对", exact=True).wait_for()
                page.get_by_role("button", name="暂停 Agent", exact=True).click()
                page.get_by_text("Agent 已暂停", exact=True).wait_for()
                assert worker["actions"] == ["start", "stop", "stop"], "cutover block must still let the owner stop back to a paused preflight"
                page.get_by_role("textbox", name="附件读取会话 ID").fill("")
                assert page.get_by_role("checkbox", name="synthetic-room@chatroom 自动总结视频号分享", exact=True).is_disabled()
                expect(page.get_by_role("checkbox", name="自动读图", exact=True)).to_be_disabled()
                expect(page.get_by_role("checkbox", name="自动读视频", exact=True)).to_be_disabled()
                with page.expect_response(lambda response: response.request.method == "PUT" and response.url.endswith("/policy")):
                    page.get_by_role("button", name="保存回复范围").click()
                page.locator("#apps-feedback").get_by_text("回复范围已保存。", exact=True).wait_for()
                assert worker["attachment_threads"] == [] and worker["media_reply_threads"] == []
                assert worker["auto_observe_images"] is False
                assert worker["auto_observe_videos"] is False
                assert worker["group_rules"] == [{"thread_id": "synthetic-room@chatroom", "note": "收到分享时先总结重点", "summarize_shares": False}]
                page.get_by_role("textbox", name="Agent 免打扰群 ID", exact=True).fill("")
                with page.expect_response(lambda response: response.request.method == "PUT" and response.url.endswith("/policy")):
                    page.get_by_role("button", name="保存回复范围").click()
                page.locator("#apps-feedback").get_by_text("回复范围已保存。", exact=True).wait_for()
                assert worker["group_mute_threads"] == []
                expect(page.get_by_role("textbox", name="Agent 免打扰群 ID", exact=True)).to_have_value("")
                assert page.get_by_role("textbox", name="Agent 免打扰群 ID", exact=True).input_value() == ""
                page.get_by_text("通讯录", exact=True).click()
                page.get_by_role("textbox", name="搜索联系人", exact=True).fill("蓝")
                page.get_by_role("button", name="查询联系人", exact=True).click()
                page.get_by_text("蓝色大肥鱼 · wxid-synthetic-1", exact=True).wait_for()
                page.get_by_text("已读取前 2 位匹配联系人 · 本页 2 条，每页最多 50 条", exact=True).wait_for()
                page.get_by_role("textbox", name="搜索联系人", exact=True).fill("不应影响下一页")
                page.get_by_role("spinbutton", name="联系人每页数量", exact=True).fill("99")
                page.get_by_role("button", name="下一页", exact=True).click()
                page.get_by_text("蓝鲸 · wxid-synthetic-3", exact=True).wait_for()
                assert page.get_by_text("蓝色大肥鱼 · wxid-synthetic-1", exact=True).count() == 0
                page.get_by_role("textbox", name="搜索联系人", exact=True).fill("蓝")
                page.get_by_role("spinbutton", name="联系人每页数量", exact=True).fill("500")
                page.get_by_role("button", name="查询联系人", exact=True).click()
                page.get_by_text("大页联系人 · wxid-synthetic-4", exact=True).wait_for()
                page.get_by_text("本页 1 条，每页最多 200 条 · 原生接口单页上限为 200 条，可用续读位置继续。", exact=False).wait_for()
                assert len(contact_queries) == 3

                groups_pattern = re.compile(r".*/api/apps/wechat/connections/synthetic-wechat/groups(?:\?.*)?$")
                group_notification_pattern = re.compile(r".*/api/apps/wechat/connections/synthetic-wechat/groups/.+/notification$")
                group_profile_pattern = re.compile(r".*/api/apps/wechat/connections/synthetic-wechat/groups/.+/profile$")
                group_actions_pattern = status_pattern + "/group-actions"
                group_queries, group_actions, group_reads = [], [], {}
                group_state = {"muted": False, "operation": None, "name": "合成测试群", "rename_operation": None,
                               "my_display_name": "合成群昵称", "my_name_operation": None}

                def group_directory(route):
                    assert route.request.method == "GET"
                    assert "/synthetic-wechat/groups" in route.request.url
                    query = parse_qs(urlparse(route.request.url).query, keep_blank_values=True)
                    group_queries.append(query)
                    assert query["search"] == ["测试"]
                    assert query["limit"] == ["50"]
                    if query.get("cursor") == ["synthetic-groups-next"]:
                        fulfill_json({"items": [{"id": "unknown-room@chatroom", "display_name": "未知状态群"}], "has_more": False,
                                      "effective_limit": 50, "coverage": "已读取 2 个匹配群聊"})(route)
                    else:
                        assert "cursor" not in query
                        fulfill_json({"items": [{"id": "synthetic-room@chatroom", "display_name": "合成测试群"}],
                                      "next_cursor": "synthetic-groups-next", "has_more": True, "effective_limit": 50,
                                      "coverage": "已读取前 1 个匹配群聊"})(route)

                def group_notification(route):
                    assert route.request.method == "GET"
                    path = unquote(urlparse(route.request.url).path)
                    group_id = path.split("/groups/", 1)[1].rsplit("/notification", 1)[0]
                    group_reads[group_id] = group_reads.get(group_id, 0) + 1
                    if group_id == "synthetic-room@chatroom":
                        if group_reads[group_id] >= 2 and group_state["operation"]:
                            group_state.update(muted=True, operation={"operation_id": "group-mute-1", "state": "succeeded", "lookup": "wechat-receipt"})
                        fulfill_json({"muted": group_state["muted"], "can_change": True, "operation": group_state["operation"]})(route)
                        return
                    assert group_id == "unknown-room@chatroom"
                    fulfill_json({"can_change": False, "coverage": "unavailable",
                                  "operation": {"operation_id": "group-mute-unknown", "state": "submission_unknown", "lookup": "native-lookup"}})(route)

                def group_profile(route):
                    assert route.request.method == "GET"
                    path = unquote(urlparse(route.request.url).path)
                    group_id = path.split("/groups/", 1)[1].rsplit("/profile", 1)[0]
                    if group_id == "synthetic-room@chatroom":
                        if group_state["rename_operation"] and group_state["rename_operation"]["state"] == "submitted_pending":
                            group_state.update(name="新的合成群名", rename_operation={"operation_id": "group-rename-1", "state": "succeeded", "lookup": "wechat-receipt"})
                        if group_state["my_name_operation"] and group_state["my_name_operation"]["state"] == "submitted_pending":
                            group_state.update(my_display_name="", my_name_operation={"operation_id": "group-my-name-1", "state": "succeeded", "lookup": "wechat-receipt"})
                        fulfill_json({"name": group_state["name"], "can_rename": True, "coverage": "cached", "operation": group_state["rename_operation"],
                                      "my_display_name": group_state["my_display_name"], "can_set_my_name": True,
                                      "my_name_operation": group_state["my_name_operation"]})(route)
                        return
                    assert group_id == "unknown-room@chatroom"
                    fulfill_json({"can_rename": False, "can_set_my_name": False, "coverage": "unavailable",
                                  "operation": {"operation_id": "group-rename-unknown", "state": "submission_unknown", "lookup": "native-lookup"},
                                  "my_name_operation": {"operation_id": "group-my-name-unknown", "state": "submission_unknown", "lookup": "native-lookup"}})(route)

                def group_action(route):
                    assert route.request.method == "POST"
                    assert route.request.headers.get("x-csrf-token")
                    body = route.request.post_data_json
                    assert body["group_id"] == "synthetic-room@chatroom"
                    assert re.fullmatch(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", body["request_key"])
                    group_actions.append(body)
                    if body.get("action") == "rename":
                        assert body == {"action": "rename", "group_id": "synthetic-room@chatroom", "name": "新的合成群名", "request_key": body["request_key"]}
                        group_state["rename_operation"] = {"operation_id": "group-rename-1", "state": "submitted_pending", "lookup": "native-queued"}
                        fulfill_json(group_state["rename_operation"])(route)
                        return
                    if body.get("action") == "my_name":
                        assert body == {"action": "my_name", "group_id": "synthetic-room@chatroom", "name": "", "request_key": body["request_key"]}
                        group_state["my_name_operation"] = {"operation_id": "group-my-name-1", "state": "submitted_pending", "lookup": "native-queued"}
                        fulfill_json(group_state["my_name_operation"])(route)
                        return
                    assert body["muted"] is True
                    group_state["operation"] = {"operation_id": "group-mute-1", "state": "submitted_pending", "lookup": "native-queued"}
                    fulfill_json(group_state["operation"])(route)

                page.route(groups_pattern, group_directory)
                page.route(group_notification_pattern, group_notification)
                page.route(group_profile_pattern, group_profile)
                page.route(group_actions_pattern, group_action)
                page.get_by_text("群聊", exact=True).click()
                page.get_by_role("textbox", name="搜索群聊", exact=True).fill("测试")
                page.get_by_role("button", name="查询群聊", exact=True).click()
                page.get_by_role("button", name="合成测试群 · synthetic-room@chatroom", exact=True).click()
                page.get_by_text("当前为正常提醒", exact=True).wait_for()
                page.get_by_role("button", name="设为免打扰", exact=True).click()
                page.get_by_text("将“合成测试群”设为免打扰。", exact=True).wait_for()
                page.get_by_role("button", name="确认提交", exact=True).click()
                page.get_by_text("已提交，等待微信确认 · 操作 group-mute-1", exact=True).wait_for()
                assert len(group_actions) == 1
                page.get_by_role("button", name="刷新状态", exact=True).click()
                page.get_by_text("当前为免打扰", exact=True).wait_for()
                page.get_by_text("已确认完成 · 操作 group-mute-1", exact=True).wait_for()
                assert len(group_actions) == 1
                page.get_by_text("群名称", exact=True).wait_for()
                page.get_by_role("button", name="修改群名称", exact=True).click()
                page.get_by_role("textbox", name="新的群名称", exact=True).fill("新的合成群名")
                page.get_by_text("将“合成测试群”改为“新的合成群名”。", exact=True).wait_for()
                page.get_by_role("button", name="确认改名", exact=True).click()
                page.get_by_text("已提交，等待微信确认 · 操作 group-rename-1", exact=True).wait_for()
                assert len(group_actions) == 2
                page.get_by_role("button", name="刷新群资料", exact=True).click()
                page.get_by_text("新的合成群名（来自本机缓存）", exact=True).wait_for()
                page.get_by_text("已确认完成 · 操作 group-rename-1", exact=True).wait_for()
                assert len(group_actions) == 2
                page.get_by_text("我的群昵称", exact=True).wait_for()
                page.get_by_text("合成群昵称（来自本机缓存）", exact=True).wait_for()
                page.get_by_role("button", name="修改我的群昵称", exact=True).click()
                page.get_by_role("textbox", name="新的群昵称", exact=True).fill("")
                page.get_by_text("将“合成测试群”中的我的群昵称清空。", exact=True).wait_for()
                page.get_by_role("button", name="确认修改昵称", exact=True).click()
                page.get_by_text("已提交，等待微信确认 · 操作 group-my-name-1", exact=True).wait_for()
                assert len(group_actions) == 3
                page.get_by_role("button", name="刷新群资料", exact=True).click()
                page.get_by_text("未设置（来自本机缓存）", exact=True).wait_for()
                page.get_by_text("已确认完成 · 操作 group-my-name-1", exact=True).wait_for()
                assert len(group_actions) == 3
                page.locator(".group-directory").get_by_role("button", name="下一页", exact=True).click()
                page.get_by_role("button", name="未知状态群 · unknown-room@chatroom", exact=True).click()
                page.get_by_text("当前免打扰状态未取得，可刷新后再决定。", exact=True).wait_for()
                page.get_by_text("提交结果待核对 · 操作 group-mute-unknown", exact=True).wait_for()
                assert page.get_by_role("button", name="设为免打扰", exact=True).count() == 0
                page.get_by_text("当前连接不能修改这个群的免打扰；仍可刷新查看已有操作回执。", exact=True).wait_for()
                page.get_by_text("提交结果待核对 · 操作 group-rename-unknown", exact=True).wait_for()
                assert page.get_by_role("button", name="修改群名称", exact=True).count() == 0
                page.get_by_text("这次改名仍在核对中；刷新只查询原操作，不会重复提交。", exact=True).wait_for()
                page.get_by_text("提交结果待核对 · 操作 group-my-name-unknown", exact=True).wait_for()
                assert page.get_by_role("button", name="修改我的群昵称", exact=True).count() == 0
                page.get_by_text("这次群昵称修改仍在核对中；刷新只查询原操作，不会重复提交。", exact=True).wait_for()
                assert len(group_queries) == 2
                page.set_viewport_size({"width": 375, "height": 780})
                assert_no_horizontal_overflow(page, "WeChat group directory")
                assert_worker_controls_fit(page)
                page.screenshot(path=str(output / "apps-groups-narrow.png"), full_page=True)
                page.set_viewport_size({"width": 1280, "height": 800})
                page.get_by_text("群聊", exact=True).click()
                page.get_by_text("通讯录", exact=True).click()
                share = {"installed": False, "enabled": False, "runtime_ready": True, "revision": 0, "configuration": {"revision": 0, "values": {}, "configured_secrets": []}}
                def share_settings(route):
                    if route.request.method == "PUT":
                        assert route.request.headers.get("x-csrf-token")
                        body = route.request.post_data_json
                        if body["action"] == "install":
                            assert body["grant_network"] is True
                            share.update(installed=True, revision=1)
                        else:
                            assert body["revision"] == share["revision"]
                            if body["action"] == "configure":
                                config = body["configuration"]
                                assert config["secrets"] == {"yuanbao_cookie": "synthetic-browser-credential"}
                                share["configuration"] = {"revision": 1, "values": config["values"], "configured_secrets": ["yuanbao_cookie"]}
                            else:
                                share.update(enabled=body["action"] == "enable", revision=share["revision"] + 1)
                    fulfill_json(share)(route)
                page.route(status_pattern + "/share-reader", share_settings)
                page.get_by_text("视频号解析", exact=True).click()
                page.get_by_role("button", name="安装并授予受控联网权限").click()
                page.get_by_label("元宝解析凭据", exact=True).fill("synthetic-browser-credential")
                page.get_by_role("button", name="保存解析设置").click()
                page.get_by_placeholder("已配置，留空保留").wait_for()
                assert page.get_by_label("元宝解析凭据", exact=True).input_value() == ""
                page.set_viewport_size({"width": 375, "height": 780})
                assert_no_horizontal_overflow(page, "share reader configuration")
                assert_worker_controls_fit(page)
                page.screenshot(path=str(output / "apps-share-reader-narrow.png"), full_page=True)
                page.set_viewport_size({"width": 1280, "height": 800})
                page.get_by_role("button", name="启用解析", exact=True).click()
                page.get_by_text("已启用，按需运行", exact=True).wait_for()
                page.get_by_role("button", name="停用解析", exact=True).click()
                page.get_by_text("已停用", exact=True).wait_for()
                page.get_by_text("视频号解析", exact=True).click()
                media = {"installed": False, "enabled": False, "runtime_ready": True, "revision": 0}

                def media_settings(route):
                    if route.request.method == "PUT":
                        assert route.request.headers.get("x-csrf-token")
                        body = route.request.post_data_json
                        if body["action"] == "install":
                            assert body["grant_media"] is True
                            media.update(installed=True, revision=1)
                        elif body["action"] == "configure":
                            assert body["revision"] == media["revision"]
                            config = body["configuration"]
                            if config["values"].get("asr_backend") == "local":
                                assert config["values"] == {"vision_endpoint": "https://vision.example/chat/completions", "vision_model": "synthetic-vision",
                                                            "asr_endpoint": "https://asr.example/transcriptions", "asr_model": "synthetic-asr", "asr_backend": "local"}
                                assert config["secrets"] == {}
                                media["configuration"] = {"revision": 1, "values": config["values"], "configured_secrets": ["vision_key", "asr_key"]}
                            else:
                                assert config["values"] == {"vision_endpoint": "https://vision.example/chat/completions", "vision_model": "synthetic-vision",
                                                            "asr_endpoint": "https://asr.example/transcriptions", "asr_model": "synthetic-asr", "asr_backend": "remote"}
                                assert config["secrets"] == {"vision_key": "synthetic-media-browser-key", "asr_key": "synthetic-asr-browser-key"}
                                media["configuration"] = {"revision": 1, "values": config["values"], "configured_secrets": ["vision_key", "asr_key"]}
                        else:
                            assert body["revision"] == media["revision"]
                            media.update(enabled=body["action"] == "enable", revision=media["revision"] + 1)
                    if media.get("configuration", {}).get("values", {}).get("asr_backend") == "local":
                        media["local_asr_ready"] = False
                    fulfill_json(media)(route)

                page.route(status_pattern + "/media-understanding", media_settings)
                page.get_by_text("媒体理解", exact=True).click()
                page.get_by_role("button", name="安装并授权媒体处理和模型调用").click()
                page.get_by_label("视觉接口地址", exact=True).fill("https://vision.example/chat/completions")
                page.get_by_label("视觉模型", exact=True).fill("synthetic-vision")
                page.get_by_label("视觉接口密钥", exact=True).fill("synthetic-media-browser-key")
                page.get_by_label("语音识别接口地址", exact=True).fill("https://asr.example/transcriptions")
                page.get_by_label("语音识别模型", exact=True).fill("synthetic-asr")
                page.get_by_label("语音识别接口密钥", exact=True).fill("synthetic-asr-browser-key")
                page.get_by_role("button", name="保存媒体设置").click()
                key_field = page.get_by_label("视觉接口密钥", exact=True).and_(page.get_by_placeholder("已配置，留空保留"))
                key_field.wait_for()
                assert key_field.input_value() == ""
                page.set_viewport_size({"width": 375, "height": 780})
                assert_no_horizontal_overflow(page, "media configuration")
                assert_worker_controls_fit(page)
                page.screenshot(path=str(output / "apps-media-understanding-narrow.png"), full_page=True)
                page.set_viewport_size({"width": 1280, "height": 800})
                page.get_by_role("button", name="启用媒体理解", exact=True).click()
                page.get_by_role("button", name="停用媒体理解", exact=True).click()
                page.get_by_role("button", name="启用媒体理解", exact=True).wait_for()
                page.get_by_label("语音识别方式", exact=True).select_option("local")
                page.get_by_text("使用主人已登记的本地媒体识别；不会改用远程接口。", exact=True).wait_for()
                assert page.get_by_label("语音识别接口地址", exact=True).is_hidden(), "local ASR must hide only remote ASR fields"
                assert page.get_by_label("视觉接口地址", exact=True).input_value() == "https://vision.example/chat/completions", "switching ASR must retain visual configuration"
                with page.expect_response(lambda response: response.request.method == "PUT" and response.url.endswith("/media-understanding")):
                    page.get_by_role("button", name="保存媒体设置").click()
                expect(page.get_by_role("button", name="保存媒体设置")).to_be_enabled()
                assert media["configuration"]["values"]["asr_backend"] == "local"
                expect(page.get_by_text("请先检查项目工具中的本地媒体识别资源。", exact=True)).to_be_visible()
                page.set_viewport_size({"width": 375, "height": 780})
                assert_no_horizontal_overflow(page, "local ASR resource warning")
                page.screenshot(path=str(output / "apps-local-asr-unavailable-narrow.png"), full_page=True)
                page.set_viewport_size({"width": 1280, "height": 800})
                page.get_by_label("语音识别方式", exact=True).select_option("remote")
                assert page.get_by_label("语音识别接口地址", exact=True).input_value() == "https://asr.example/transcriptions", "switching back must retain remote ASR settings"
                page.get_by_text("媒体理解", exact=True).click()
                # Reading delivery history and receipt checks must never post a send.
                receipt_state = {"version": "first", "reads": 0, "checks": 0, "stops": 0, "resumes": 0, "continuation": None,
                                 "recall_queries": [], "recall_posts": []}
                receipts_pattern = re.compile(r"/api/apps/wechat/connections/synthetic-wechat/receipts(?:[/?].*)?$")
                pending_receipt = {"idempotency_key": "synthetic-send", "kind": "voice", "resumed_from": "synthetic-original",
                    "conversation_id": "synthetic-room@chatroom", "state": "submission_unknown",
                    "lookup": "stored", "operation_id": "synthetic-job", "accepted_parts": [{"index": 0, "server_id": "101"}, {"index": 1, "server_id": "102"}],
                    "unconfirmed_parts": [{"index": 2, "state": "submitting"}, {"index": 3, "state": "cancelled"}]}
                accepted_receipt = {"idempotency_key": "synthetic-direct", "kind": "file", "conversation_id": "synthetic-direct@chatroom",
                    "state": "accepted", "server_id": "201", "lookup": "platform"}
                def receipt_records(route):
                    parsed = urlparse(route.request.url)
                    if "/recall" in parsed.path:
                        params = parse_qs(parsed.query)
                        server_id = params.get("server_id", [""])[0]
                        cursor = params.get("cursor", [""])[0]
                        if route.request.method == "POST":
                            assert parsed.path.endswith("/receipts/synthetic-send/recall")
                            assert route.request.headers.get("x-csrf-token"), "recall lacked owner CSRF"
                            body = route.request.post_data_json
                            assert body == {"server_id": "102", "message_id": "msg_voice_part"}, "recall must target only the selected accepted part"
                            receipt_state["recall_posts"].append(body)
                            fulfill_json({"operation_id": "recall-102", "state": "submitted_pending", "action": "recall"}, status=202)(route)
                            return
                        assert route.request.method == "GET", "recall lookup must be read-only before confirmation"
                        receipt_state["recall_queries"].append((parsed.path, server_id, cursor))
                        if parsed.path.endswith("/receipts/synthetic-send/recall") and server_id == "102":
                            if cursor == "part-102-next":
                                fulfill_json({"message_id": "msg_voice_part"})(route)
                            elif any(post["server_id"] == "102" for post in receipt_state["recall_posts"]):
                                fulfill_json({"operation": {"operation_id": "recall-102", "state": "submitted_pending"}})(route)
                            else:
                                fulfill_json({"next_cursor": "part-102-next"})(route)
                            return
                        if parsed.path.endswith("/receipts/synthetic-direct/recall") and server_id == "201":
                            fulfill_json({"operation": {"operation_id": "recall-201", "state": "unknown"}})(route)
                            return
                        fulfill_json({})(route)
                        return
                    if route.request.url.endswith("/receipts/synthetic-send/cancel"):
                        assert route.request.method == "POST"
                        assert route.request.headers.get("x-csrf-token"), "stop lacked owner CSRF"
                        receipt_state["stops"] += 1
                        if receipt_state["continuation"]:
                            receipt_state["continuation"] = {"state": "cancelled", "message": "续办已停止"}
                        fulfill_json({"state": "cancellation_requested", "scope": "originating_turn"}, status=202)(route)
                        return
                    if route.request.url.endswith("/receipts/synthetic-send/resume"):
                        assert route.request.method == "POST"
                        assert route.request.headers.get("x-csrf-token"), "resume lacked owner CSRF"
                        assert route.request.post_data_json["request_key"]
                        receipt_state["resumes"] += 1
                        receipt_state["continuation"] = {"state": "requested", "message": "续办请求已保存"}
                        fulfill_json({"continuation": receipt_state["continuation"]}, status=202)(route)
                        return
                    assert route.request.method == "GET", "receipt view attempted a mutation"
                    if route.request.url.endswith("/receipts/synthetic-continuation"):
                        fulfill_json({"idempotency_key": "synthetic-continuation", "kind": "voice", "conversation_id": "synthetic-room@chatroom", "state": "accepted", "lookup": "platform", "resumed_from": "synthetic-send"})(route)
                        return
                    if "/receipts/synthetic-send" in route.request.url:
                        receipt_state["checks"] += 1
                        if receipt_state["continuation"] and receipt_state["continuation"]["state"] == "requested":
                            receipt_state["continuation"] = {"state": "tracking", "message": "已建立续发任务", "delivery_key": "synthetic-continuation"}
                        fulfill_json(dict(pending_receipt, state="cancelled", cancel_requested=True, lookup="platform", continuation=receipt_state["continuation"]))(route)
                    else:
                        receipt_state["reads"] += 1
                        if "after=synthetic-send" in route.request.url:
                            receipt_state["version"] = "changed"
                        fulfill_json({"items": [pending_receipt, accepted_receipt], "snapshot": receipt_state["version"],
                            "next_after": "synthetic-send" if receipt_state["version"] == "first" else "",
                            "coverage": "current_worker_checkpoint"})(route)
                page.route(receipts_pattern, receipt_records)
                page.get_by_text("投递记录", exact=True).click()
                page.get_by_role("button", name="查看投递记录", exact=True).click()
                page.get_by_text(re.compile("语音续发.*发送结果待核对")).wait_for()
                page.get_by_role("button", name="继续查看", exact=True).click()
                page.get_by_text("投递记录已更新，请重新查看。", exact=True).wait_for()
                page.get_by_role("button", name="重新查看", exact=True).click()
                # A delivery can be recalled only after its server receipt is located. For multipart sends,
                # choose exactly one accepted part, never submit a bulk recall.
                multipart_row = page.locator(".work-record").filter(has_text="synthetic-room@chatroom")
                part_picker = multipart_row.get_by_label("选择要撤回的已确认消息", exact=True)
                part_picker.select_option("102")
                multipart_row.get_by_role("button", name="定位可撤回消息", exact=True).click()
                page.get_by_role("button", name="继续查找", exact=True).wait_for()
                assert not receipt_state["recall_posts"], "read-only lookup must not recall a message"
                page.get_by_role("button", name="继续查找", exact=True).click()
                page.get_by_role("button", name="确认撤回", exact=True).wait_for()
                page.get_by_role("button", name="确认撤回", exact=True).click()
                page.get_by_role("button", name="核对撤回状态", exact=True).wait_for()
                assert receipt_state["recall_posts"] == [{"server_id": "102", "message_id": "msg_voice_part"}]
                # Pending and unknown operations remain read-only lookup states; neither may create a duplicate POST.
                page.get_by_role("button", name="核对撤回状态", exact=True).click()
                direct_row = page.locator(".work-record").filter(has_text="synthetic-direct@chatroom")
                direct_row.get_by_role("button", name="定位可撤回消息", exact=True).click()
                direct_row.get_by_role("button", name="核对撤回状态", exact=True).wait_for()
                assert len(receipt_state["recall_posts"]) == 1
                assert any(path.endswith("/receipts/synthetic-send/recall") and server == "102" for path, server, _ in receipt_state["recall_queries"])
                assert any(path.endswith("/receipts/synthetic-direct/recall") and server == "201" for path, server, _ in receipt_state["recall_queries"])
                multipart_row.get_by_role("button", name="停止本回合投递", exact=True).click()
                multipart_row.get_by_role("button", name="停止意图已保存", exact=True).wait_for()
                assert multipart_row.get_by_role("button", name="停止意图已保存", exact=True).is_disabled()
                multipart_row.get_by_role("button", name="核对回执", exact=True).click()
                page.get_by_text(re.compile("语音.*正在停止，等待回执.*已接受 2 段.*待核对 1 段.*已取消 1 段")).wait_for()
                assert receipt_state["reads"] == 3 and receipt_state["checks"] == 1 and receipt_state["stops"] == 1
                page.get_by_role("button", name="续发未发送片段", exact=True).click()
                page.get_by_role("button", name="续办请求已保存", exact=True).wait_for()
                assert page.get_by_role("button", name="续办请求已保存", exact=True).is_disabled()
                multipart_row.get_by_role("button", name="核对回执", exact=True).click()
                page.get_by_text(re.compile("语音.*续发结果：.*微信已接受")).wait_for()
                multipart_row.get_by_role("button", name="停止续办", exact=True).click()
                page.get_by_text(re.compile("语音.*续办已停止")).wait_for()
                assert receipt_state["resumes"] == 1 and receipt_state["stops"] == 2
                artifact_id = "work-" + "a" * 64
                delivery_id = "export-" + "b" * 64
                share_requests = []
                def register_artifact(route):
                    assert route.request.post_data_json["path"] == "作品/合成文档.pdf"
                    fulfill_json({"artifact_id": artifact_id, "filename": "合成文档.pdf", "status": "registered"})(route)
                def work_grants(route):
                    if route.request.method == "POST":
                        body = route.request.post_data_json
                        assert body["artifact_id"] == artifact_id and body["thread_id"] == "synthetic-room@chatroom"
                        assert body["requester_id"] == "synthetic-person" and body["enabled"] is True
                        share_requests.append(body)
                        fulfill_json({"delivery_id": delivery_id, "enabled": True})(route)
                    else:
                        fulfill_json({"items": [{"delivery_id": delivery_id, "thread_id": "synthetic-room@chatroom", "requester_id": "synthetic-person", "enabled": True}]})(route)
                artifact_pattern = "**/api/local/artifacts"
                grants_pattern = status_pattern + "/work-grants*"
                page.route(artifact_pattern, register_artifact)
                page.route(grants_pattern, work_grants)
                close_application(page)
                page.get_by_text("登记家目录中的作品", exact=True).click()
                page.get_by_label("成品文件", exact=True).fill("作品/合成文档.pdf")
                page.get_by_role("button", name="登记作品", exact=True).click()
                page.locator("#artifact-result").filter(has_text=artifact_id).wait_for()
                open_application(page)
                page.get_by_text("授权作品交付", exact=True).click()
                page.get_by_label("作品编号", exact=True).fill(artifact_id)
                page.get_by_label("接收会话", exact=True).fill("synthetic-room@chatroom")
                page.get_by_label("请求者", exact=True).fill("synthetic-person")
                page.get_by_role("button", name="允许交付", exact=True).click()
                page.locator("#apps-feedback").filter(has_text="作品交付已授权").wait_for()
                page.get_by_role("button", name="查看已有授权", exact=True).click()
                page.get_by_role("button", name="已列出全部", exact=True).wait_for()
                assert len(share_requests) == 1
                page.set_viewport_size({"width": 375, "height": 780})
                assert_no_horizontal_overflow(page, "worker controls")
                assert_worker_controls_fit(page)
                page.screenshot(path=str(output / "apps-worker-narrow.png"), full_page=True)
                page.set_viewport_size({"width": 1280, "height": 800})
                page.unroute(artifact_pattern, register_artifact)
                page.unroute(grants_pattern, work_grants)
                page.unroute(receipts_pattern, receipt_records)
                page.unroute(worker_policy_pattern, save_worker)
                page.unroute(worker_lifecycle_pattern, set_worker)
                page.unroute(contacts_pattern, contact_directory)

                # An unavailable account read remains a visible, recoverable application state.
                page.unroute(connection_base, setup_connections)
                unavailable_route = fulfill_json({"error": "synthetic connection unavailable"}, status=503)
                page.route("**/api/apps/wechat/connections", unavailable_route)
                refresh_application(page)
                page.get_by_text("账号状态暂不可读", exact=True).wait_for()
                page.unroute("**/api/apps/wechat/connections", unavailable_route)
                page.unroute("**/api/apps", apps_route)
                page.unroute(status_pattern, status_route)

                home_tab(page, "settings")
                assert page.locator("#model-name").input_value() == "synthetic-unsaved-draft", "application state replaced the model draft"
                home_tab(page, "chat")
                page.screenshot(path=str(output / "ready-light.png"), full_page=True)
                home_tab(page, "settings")
                page.locator("#settings-access-button").click()
                page.get_by_text("工作空间已暂停", exact=True).wait_for()
                assert page.locator("#model-name").input_value() == "synthetic-unsaved-draft", "workspace change replaced the model draft"
                home_tab(page, "chat")
                assert page.locator("#chat-input").is_enabled(), "workspace pause must not disable local chat"
                page.reload()
                page.get_by_text("工作空间已暂停", exact=True).wait_for()
                session = context.request.get(origin + "/api/session").json()
                stale = context.request.post(origin + "/api/local/home", data={"action": "authorize", "revision": initial["revision"]}, headers={"Origin": origin, "X-CSRF-Token": session["csrf"]})
                assert stale.status == 409, "stale revision revived grant"
                page.set_viewport_size({"width": 375, "height": 812})
                page.emulate_media(color_scheme="dark")
                assert_no_horizontal_overflow(page, "settings")
                page.screenshot(path=str(output / "paused-dark-375.png"), full_page=True)
                page.get_by_role("tab", name="应用").click()
                page.locator("#apps-view").wait_for(state="visible")
                assert_no_horizontal_overflow(page, "apps")
                page.screenshot(path=str(output / "apps-dark-375.png"), full_page=True)
                home_tab(page, "settings")
                page.locator("#settings-access-button").click()
                page.get_by_text("工作空间已授权", exact=True).wait_for()
                page.locator("#settings-logout-button").click()
                page.locator("#login-view").wait_for(state="visible")
                assert context.request.get(origin + "/api/local/home").status == 401
                stop(process)
                process, origin = start(binary, data, workspace)
                page.goto(origin)
                page.locator("#login-password").fill("synthetic-home-owner")
                page.locator("#login-submit").click()
                page.locator("#chat-view").wait_for(state="visible")
                latest = context.request.get(origin + "/api/local/home").json()
                assert latest["fish_id"] == initial["fish_id"]
                assert latest["owner_id"] == initial["owner_id"]
                assert latest["directory"] == initial["directory"]
                assert note.read_text(encoding="utf-8") == "a persistent companion home"

                artifact_id = "work-" + "c" * 64
                def synthetic_chat(route):
                    if route.request.method != "GET":
                        route.continue_()
                        return
                    route.fulfill(content_type="application/json", body=json.dumps({"items": [
                        {"id": "synthetic-user", "role": "user", "text": "把成品给我", "time": "2026-11-06T09:00:00+08:00"},
                        {"id": "synthetic-assistant", "role": "assistant", "text": "已经做好：" + artifact_id, "time": "2026-11-06T09:00:02+08:00"},
                    ], "has_more": False, "busy": False}, ensure_ascii=False))
                chat_pattern = re.compile(r".*/api/local/chat\?.*")
                page.route(chat_pattern, synthetic_chat)
                page.reload()
                page.locator("#chat-view").wait_for(state="visible")
                download = page.get_by_role("link", name="下载已登记成品", exact=True)
                assert artifact_id in page.locator("#chat-stream").inner_text(), "assistant text must remain readable"
                assert download.get_attribute("href") == "/api/local/artifacts/" + artifact_id + "/download"
                page.unroute(chat_pattern, synthetic_chat)
                assert not errors, errors
                browser.close()
                print(json.dumps({"status": "passed", "model_calls": 0, "wechat_calls": 0, "scenes": ["login", "initialize", "project tool install enable disable upgrade", "artifact download link", "application catalog", "application source install or unavailable source", "synthetic application account", "synthetic QR retry and expiry", "legacy import preview confirm and stale retry", "legacy memory inventory and pagination", "legacy group note preview and confirmation", "worker policy and lifecycle", "WeChat contacts and independent grant", "WeChat group management receipts", "share reader settings", "media understanding settings", "delivery snapshot pagination", "partial cancellation receipt lookup", "artifact registration and sharing", "375 worker editor", "application account unavailable", "pause", "stale revision", "resume", "logout", "restart", "375 dark"]}, ensure_ascii=False))
        finally:
            stop(process)



def verify_douyin_authorization(binary, output):
    """Replay the app bridge without any requests to Douyin."""
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="synthetic-douyin-auth-", dir=output) as directory:
        root = Path(directory).resolve()
        process, origin = start(binary, root / "host", root / "workspace")
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(channel="msedge", headless=True)
                for desktop in (True, False):
                    width = 375 if desktop else 1280
                    context = browser.new_context(viewport={"width": width, "height": 800}, color_scheme="dark" if desktop else "light")
                    page = context.new_page()
                    if desktop:
                        page.add_init_script("""(() => {
                            const bridge = new EventTarget();
                            window.fixtureOpenRequests = [];
                            bridge.postMessage = message => window.fixtureOpenRequests.push(message);
                            window.chrome.webview = bridge;
                        })();""")
                    page.route("https://open.douyin.com/**", lambda route: route.abort())
                    page.route("**/api/apps", fulfill_json({"items": [{"id": "douyin", "name": "抖音"}]}))
                    page.route("**/api/apps/douyin/connections", fulfill_json({"items": []}))
                    attempts, cancelled, completed, refreshed = [], [], [], []
                    initial = {"revision": "", "configured": False, "status": "unconfigured", "label": "", "works_authorized": False, "publish_authorized": False}
                    verified = {"revision": "fixture-revision", "configured": True, "status": "ready", "label": "Synthetic Douyin account", "works_authorized": desktop, "publish_authorized": desktop, "refresh_available": True}
                    renewed = {**verified, "revision": "fixture-renewed", "expires_at": "2030-02-01T00:00:00Z"}
                    expired = {**renewed, "status": "expired", "refresh_available": False}
                    authorization_view = initial if desktop else verified
                    redirect = "https://owner.example.test/callback"

                    def authorization(route):
                        nonlocal authorization_view
                        if route.request.method == "GET":
                            fulfill_json(authorization_view)(route)
                            return
                        body = route.request.post_data_json
                        if body["action"] == "begin":
                            assert body["revision"] == authorization_view["revision"]
                            assert body.get("read_works") is desktop, "the optional works-list grant must match the checkbox"
                            assert body.get("publish_video") is desktop, "the optional video publishing grant must match the checkbox"
                            state = f"{len(attempts) + 1:048x}"
                            attempts.append(state)
                            scopes = "user_info,video.list.bind,video.create.bind,posting.behavior" if desktop else "user_info"
                            query = urlencode({"client_key": "fixture-client", "response_type": "code", "scope": scopes,
                                               "redirect_uri": redirect, "state": state})
                            fulfill_json({"authorize_url": "https://open.douyin.com/platform/oauth/connect?" + query,
                                          "expires_at": "2030-01-01T00:00:00Z"})(route)
                        elif body["action"] == "cancel":
                            cancelled.append(body["state"])
                            fulfill_json(initial)(route)
                        elif body["action"] == "complete":
                            completed.append(body["callback_url"])
                            authorization_view = verified
                            fulfill_json(verified)(route)
                        elif body["action"] == "refresh":
                            assert body == {"action": "refresh", "revision": "fixture-revision"}
                            refreshed.append(body)
                            authorization_view = renewed
                            fulfill_json(renewed)(route)
                        else:
                            raise AssertionError("unexpected action")

                    page.route("**/api/local/apps/douyin/authorization", authorization)
                    accepted_job, pending_job = "a" * 64, "b" * 64
                    publication_actions = []
                    target = {"app_id": "douyin", "connection_id": "authorized-account", "account_id": "synthetic-owner", "installation_id": "douyin-search"}
                    accepted = {"job_id": accepted_job, "state": "accepted", "remote_id": "7000000000000000001",
                                "target": target, "available_actions": ["status"], "progress": {"stage": "accepted", "confirmed_parts": 1, "total_parts": 1}}
                    pending = {"job_id": pending_job, "state": "awaiting_receipt", "target": target, "available_actions": ["status", "cancel"],
                               "progress": {"stage": "part_pending", "confirmed_parts": 1, "total_parts": 3}}

                    def publication_status(route):
                        body = route.request.post_data_json
                        assert body == {"action": "status", "job_id": accepted_job}
                        publication_actions.append(body)
                        accepted.update(state="published", available_actions=[])
                        fulfill_json(accepted)(route)

                    def publication_list(route):
                        items = [accepted, pending] if desktop and completed else []
                        fulfill_json({"tasks": {"items": items, "next": 0, "has_more": False}, "targets": [target] if items else []})(route)

                    page.route(re.compile(r".*/api/local/publications\?app_id=douyin(?:&.*)?$"), publication_list)
                    page.route("**/api/local/publications", publication_status)
                    page.goto(origin)
                    page.locator("#login-password").fill("synthetic-home-owner")
                    page.locator("#login-submit").click()
                    if desktop:
                        page.locator("#initialize-button").click()
                    home_tab(page, "apps")
                    if not desktop:
                        page.locator("#apps-list[aria-busy='false']").wait_for()
                        open_application(page, "抖音")
                        page.locator("#app-dialog").get_by_text(verified["label"], exact=True).wait_for()
                        page.get_by_text("请在 Windows 桌面端扫码连接抖音账号。", exact=True).wait_for()
                        assert page.get_by_text("扫码授权", exact=True).count() == 0
                        assert page.get_by_role("button", name="续期授权").count() == 0
                        assert page.get_by_role("link", name="在浏览器中打开抖音授权").count() == 0
                        assert page.get_by_label("完整回调地址").count() == 0
                        assert not attempts and not completed and not cancelled
                        context.close()
                        continue
                    page.locator("#apps-list[aria-busy='false']").wait_for()
                    open_application(page, "抖音")
                    page.get_by_text("尚未接入抖音账号", exact=True).wait_for()
                    expect(page.get_by_role("link", name="申请网站应用", exact=True)).to_have_attribute("href", "https://developer.open-douyin.com/console")
                    expect(page.get_by_role("link", name="查看申请条件", exact=True)).to_have_attribute("href", "https://partner.open-douyin.com/docs/resource/zh-CN/developer/join/issues")
                    expect(page.get_by_text("此入口需要已审核的开放平台网站应用；普通抖音网页登录不能代替这项授权。", exact=True)).to_be_visible()
                    page.locator("#app-dialog").screenshot(path=str(output / "douyin-application-required.png"))
                    page.get_by_text("扫码授权", exact=True).click()
                    works = page.get_by_label("授权读取我的作品列表")
                    publish = page.get_by_label("授权发布抖音视频")
                    assert not works.is_checked(), "works-list access must be off for a fresh authorization"
                    assert not publish.is_checked(), "video publishing access must be off for a fresh authorization"

                    def begin():
                        works.check()
                        publish.check()
                        page.get_by_label("抖音 Client Key").fill("fixture-client")
                        page.get_by_label("抖音 Client Secret").fill("fixture-private-secret")
                        page.get_by_label("抖音开放平台已登记的回调地址").fill(redirect)
                        page.get_by_role("button", name="打开扫码窗口").click()

                    def callback(state):
                        return redirect + "?" + urlencode({"code": "synthetic-code", "state": state, "scopes": "user_info,video.list.bind,video.create.bind,posting.behavior"})

                    def emit(message):
                        page.evaluate("message => chrome.webview.dispatchEvent(new MessageEvent('message', {data: message}))", message)

                    begin()
                    page.get_by_text("请在扫码窗口内扫描二维码", exact=False).wait_for()
                    assert page.evaluate("window.fixtureOpenRequests.length") == 1
                    opened = page.evaluate("window.fixtureOpenRequests[0]")
                    assert opened["type"] == "douyin-auth-open" and opened["expires_at"]
                    assert parse_qs(urlparse(opened["url"]).query)["state"] == attempts
                    assert page.get_by_role("link", name="在浏览器中打开抖音授权").count() == 0
                    assert page.get_by_label("完整回调地址").count() == 0
                    assert page.get_by_role("button", name="打开扫码窗口").is_disabled()
                    assert page.get_by_label("抖音 Client Secret").input_value() == ""
                    assert_no_horizontal_overflow(page, "375-pixel Douyin authorization")
                    page.screenshot(path=str(output / "douyin-auth-desktop-dark.png"))
                    with page.expect_request(lambda req: req.url.endswith("/douyin/authorization") and req.method == "POST"
                                             and req.post_data_json.get("action") == "cancel"):
                        emit({"type": "douyin-auth-closed", "state": attempts[0]})
                    page.get_by_text("扫码窗口已关闭，未更改当前账号连接。", exact=True).first.wait_for()
                    begin()
                    page.get_by_text("请在扫码窗口内扫描二维码", exact=False).wait_for()
                    assert page.evaluate("window.fixtureOpenRequests.length") == 2
                    emit({"type": "douyin-auth-callback", "state": attempts[0], "callback_url": callback(attempts[0])})
                    emit({"type": "douyin-auth-closed", "state": attempts[0]})
                    for _ in range(2):
                        emit({"type": "douyin-auth-callback", "state": attempts[1], "callback_url": callback(attempts[1])})
                    expected = callback(attempts[1])
                    page.locator("#app-dialog").get_by_text(verified["label"], exact=True).wait_for()
                    page.get_by_text("已授权读取我的作品列表", exact=False).wait_for()
                    page.get_by_text("已授权发布抖音视频", exact=False).wait_for()
                    with page.expect_request(lambda req: req.url.endswith("/douyin/authorization") and req.method == "POST"
                                             and req.post_data_json.get("action") == "refresh"):
                        page.get_by_role("button", name="续期授权").click()
                    page.get_by_text("抖音授权已续期。", exact=True).wait_for()
                    assert refreshed == [{"action": "refresh", "revision": "fixture-revision"}]
                    page.get_by_role("button", name="续期授权").wait_for()
                    page.get_by_text("重新扫码授权", exact=True).click()
                    assert works.is_checked(), "renewing a works grant should preserve the visible selection"
                    assert publish.is_checked(), "renewing a publishing grant should preserve the visible selection"
                    workbench = page.locator("#douyin-publication")
                    workbench.get_by_role("list", name="抖音发布任务").wait_for()
                    pending_row = workbench.locator(f'[data-job="{pending_job}"]')
                    assert pending_row.get_by_role("button", name="继续任务").count() == 0
                    assert workbench.get_by_text("核对投稿结果", exact=True).count() == 0
                    accepted_row = workbench.locator(f'[data-job="{accepted_job}"]')
                    accepted_row.get_by_role("button", name="查询状态").click()
                    accepted_row.get_by_text("已公开", exact=True).wait_for()
                    assert len(publication_actions) == 1
                    assert_no_horizontal_overflow(page, "375-pixel Douyin publication")
                    page.screenshot(path=str(output / "douyin-publication-desktop-dark.png"))
                    assert completed == [expected], "stale or repeated callback was submitted"
                    assert cancelled == [attempts[0]]
                    authorization_view = expired
                    page.reload()
                    home_tab(page, "apps")
                    page.locator("#apps-list[aria-busy='false']").wait_for()
                    open_application(page, "抖音")
                    page.get_by_text("抖音授权已过期", exact=True).wait_for()
                    workbench.get_by_text("发布授权已过期", exact=True).wait_for()
                    assert workbench.get_by_text("已授权发布视频", exact=True).count() == 0
                    workbench.locator(f'[data-job="{accepted_job}"]').get_by_text("已公开", exact=True).wait_for()
                    page.get_by_text("重新扫码授权", exact=True).wait_for()
                    page.get_by_role("button", name="打开扫码窗口").wait_for()
                    assert page.get_by_role("button", name="续期授权").count() == 0
                    assert_no_horizontal_overflow(page, "375-pixel expired Douyin authorization")
                    begin()
                    page.get_by_text("请在扫码窗口内扫描二维码", exact=False).wait_for()
                    assert len(attempts) == 3
                    emit({"type": "douyin-auth-callback", "state": attempts[2], "callback_url": callback(attempts[2])})
                    page.locator("#app-dialog").get_by_text(verified["label"], exact=True).wait_for()
                    workbench.get_by_text("已授权发布视频", exact=True).wait_for()
                    assert completed == [expected, callback(attempts[2])]
                    context.close()
                browser.close()
        finally:
            stop(process)
    print("Douyin authorization browser replay passed (Windows bridge and browser status UI, no platform requests).")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--bare-binary", type=Path, help="optional executable without a sibling connected-apps directory")
    parser.add_argument("--douyin-authorization-only", action="store_true")
    args = parser.parse_args()
    if args.douyin_authorization_only:
        verify_douyin_authorization(args.binary.resolve(), args.output.resolve())
    else:
        verify(args.binary.resolve(), args.output.resolve())
        verify_missing_source((args.bare_binary or args.binary).resolve(), args.output.resolve())
