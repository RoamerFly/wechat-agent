"""Offline acceptance for ordinary Douyin web-login UI; it never opens Douyin."""

import argparse
import json
from pathlib import Path
import re
import tempfile

from playwright.sync_api import sync_playwright

from test_local_home_ui import assert_no_horizontal_overflow, fulfill_json, home_tab, open_application, start, stop


def verify(binary, output):
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="synthetic-douyin-web-login-", dir=output) as directory:
        root = Path(directory).resolve()
        process, origin = start(binary, root / "host", root / "workspace")
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(channel="msedge", headless=True)
                for width, color, suffix in ((375, "dark", "mobile-dark"), (1280, "light", "desktop-light")):
                    context = browser.new_context(viewport={"width": width, "height": 800}, color_scheme=color)
                    page = context.new_page()
                    page.add_init_script("""(() => {
                        window.fixtureDelayWebLogin = false;
                        const nativeFetch = window.fetch.bind(window);
                        window.fetch = (...args) => {
                            const target = typeof args[0] === 'string' ? args[0] : args[0]?.url || '';
                            const response = nativeFetch(...args);
                            return String(target).includes('/api/local/apps/douyin/web-login') && window.fixtureDelayWebLogin
                              ? response.then(value => new Promise(resolve => setTimeout(() => resolve(value), 1000)))
                              : response;
                        };
                    })();""")
                    dialogs = []
                    page.on("dialog", lambda dialog: dialogs.append(dialog.type))
                    page.route(re.compile(r".*/api/local/apps/catalog$"), fulfill_json({"applications": [{"id": "douyin", "name": "抖音", "install_state": "installed", "source_available": True}]}))
                    page.route(re.compile(r".*/api/apps$"), fulfill_json({"items": [{"id": "douyin", "name": "抖音"}]}))
                    page.route("**/api/apps/douyin/connections", fulfill_json({"items": []}))
                    authorization_requests = []
                    def unexpected_authorization(route):
                        authorization_requests.append(route.request.method)
                        route.fulfill(status=500, content_type="application/json", body=json.dumps({"error": "removed UI must not call authorization"}))
                    page.route("**/api/local/apps/douyin/authorization", unexpected_authorization)
                    page.route(re.compile(r".*/api/local/publications\?app_id=douyin(?:&.*)?$"), fulfill_json({"tasks": {"items": [], "next": 0, "has_more": False}, "targets": []}))

                    revision = 1
                    begin_count = 0
                    late_401 = False
                    scope_conflict = False
                    playback_conflict = False
                    asr_ready = False
                    visual_ready = False
                    get_count = 0
                    # Older station responses did not include read_enabled; the UI must treat that as false.
                    view = {"revision": revision, "state": "unconfigured", "active": False, "available": True}
                    requests = []

                    def web_login(route):
                        nonlocal revision, view, begin_count, late_401, scope_conflict, playback_conflict, asr_ready, visual_ready, get_count
                        if route.request.method == "GET":
                            get_count += 1
                            if late_401:
                                late_401 = False
                                route.fulfill(status=401, content_type="application/json", body=json.dumps({"error": "登录状态已失效，请重新验证。"}, ensure_ascii=False))
                                return
                            fulfill_json(view)(route)
                            return
                        body = route.request.post_data_json
                        requests.append(body)
                        action = body.get("action")
                        assert body.get("revision") == revision
                        if action == "begin":
                            assert body == {"action": "begin", "revision": revision, "approve_profile": True}
                            begin_count += 1
                            revision += 1
                            view = {"revision": revision, "state": "login_open", "active": True, "available": True, "attempt_id": f"attempt-{begin_count}", "read_enabled": False, "read_scope": "home", "playback_enabled": False, "audio_enabled": False, "visual_enabled": False, "comments_enabled": False, "likes_enabled": False, "replies_enabled": False, "publish_enabled": False}
                        elif action == "confirm":
                            assert body == {"action": "confirm", "revision": revision, "attempt_id": view["attempt_id"]}
                            revision += 1
                            view = {"revision": revision, "state": "saved", "active": False, "available": True, "confirmed_at": "2030-01-02T03:04:05Z", "read_enabled": False, "read_scope": "home", "comments_enabled": False, "likes_enabled": False, "replies_enabled": False, "publish_enabled": False}
                        elif action == "set_publish":
                            assert body == {"action": "set_publish", "revision": revision, "allow_publish": True}
                            assert view["state"] == "saved" and not view["active"] and "attempt_id" not in view
                            revision += 1
                            view = {**view, "revision": revision, "publish_enabled": True}
                        elif action == "begin_creator":
                            assert body == {"action": "begin_creator", "revision": revision, "approve_profile": True}
                            begin_count += 1
                            revision += 1
                            view = {"revision": revision, "state": "login_open", "active": True, "available": True, "attempt_id": f"attempt-{begin_count}", "read_enabled": False, "read_scope": "home", "playback_enabled": False, "audio_enabled": False, "visual_enabled": False, "comments_enabled": False, "likes_enabled": False, "replies_enabled": False, "publish_enabled": False}
                        elif action == "set_read":
                            assert set(body) <= {"action", "revision", "allow_read", "read_scope", "allow_playback", "allow_audio", "allow_visual", "allow_comments"}
                            assert isinstance(body["allow_read"], bool)
                            assert "allow_playback" not in body or isinstance(body["allow_playback"], bool)
                            assert "allow_audio" not in body or isinstance(body["allow_audio"], bool)
                            assert "allow_visual" not in body or isinstance(body["allow_visual"], bool)
                            assert "allow_comments" not in body or isinstance(body["allow_comments"], bool)
                            assert sum(key in body for key in ("allow_audio", "allow_playback", "allow_visual", "allow_comments")) <= 1, "每个开关应只提交自己的许可，不覆盖另一项"
                            assert "read_scope" not in body or body["read_scope"] in ("home", "home_and_videos")
                            assert view["state"] == "saved" and not view["active"] and "attempt_id" not in view
                            if body.get("allow_audio") is True and not asr_ready:
                                route.fulfill(status=503, content_type="application/json", body=json.dumps({"error": "本机语音识别工具未就绪，请先启用并配置识别模型。"}, ensure_ascii=False))
                                return
                            if body.get("allow_visual") is True and not visual_ready:
                                route.fulfill(status=503, content_type="application/json", body=json.dumps({"error": "本机视觉工具尚未就绪，请先启用并配置媒体理解。"}, ensure_ascii=False))
                                return
                            if playback_conflict:
                                playback_conflict = False
                                assert body == {"action": "set_read", "revision": revision, "allow_read": True, "read_scope": "home_and_videos", "allow_playback": True}
                                revision += 1
                                view = {**view, "revision": revision, "playback_enabled": False}
                                route.fulfill(status=409, content_type="application/json", body=json.dumps({"error": "播放许可已在别处更新，请重新读取。"}, ensure_ascii=False))
                                return
                            if scope_conflict:
                                scope_conflict = False
                                assert body == {"action": "set_read", "revision": revision, "allow_read": True, "read_scope": "home"}
                                route.fulfill(status=409, content_type="application/json", body=json.dumps({"error": "读取范围已在别处更新，请重新读取。"}, ensure_ascii=False))
                                return
                            revision += 1
                            read_scope = body.get("read_scope", view.get("read_scope", "home")) if body["allow_read"] else "home"
                            playback_enabled = body.get("allow_playback", view.get("playback_enabled", False)) if body["allow_read"] and read_scope == "home_and_videos" else False
                            audio_enabled = body.get("allow_audio", view.get("audio_enabled", False)) if body["allow_read"] and read_scope == "home_and_videos" else False
                            visual_enabled = body.get("allow_visual", view.get("visual_enabled", False)) if body["allow_read"] and read_scope == "home_and_videos" else False
                            comments_enabled = body.get("allow_comments", view.get("comments_enabled", False)) if body["allow_read"] and read_scope == "home_and_videos" else False
                            likes_enabled = view.get("likes_enabled", False) if body["allow_read"] and read_scope == "home_and_videos" else False
                            replies_enabled = view.get("replies_enabled", False) if comments_enabled else False
                            view = {**view, "revision": revision, "read_enabled": body["allow_read"], "read_scope": read_scope, "playback_enabled": playback_enabled, "audio_enabled": audio_enabled, "visual_enabled": visual_enabled, "comments_enabled": comments_enabled, "likes_enabled": likes_enabled, "replies_enabled": replies_enabled}
                        elif action == "set_interactions":
                            assert set(body) <= {"action", "revision", "allow_likes", "allow_replies"}
                            assert any(key in body for key in ("allow_likes", "allow_replies"))
                            assert all(isinstance(body[key], bool) for key in ("allow_likes", "allow_replies") if key in body)
                            assert view["state"] == "saved" and not view["active"] and "attempt_id" not in view
                            assert view.get("read_enabled") is True and view.get("read_scope") == "home_and_videos"
                            if body.get("allow_replies") is True:
                                assert view.get("comments_enabled") is True, "回复评论必须保留评论读取依赖"
                            revision += 1
                            view = {**view, "revision": revision,
                                    "likes_enabled": body.get("allow_likes", view.get("likes_enabled", False)),
                                    "replies_enabled": body.get("allow_replies", view.get("replies_enabled", False))}
                        elif action == "clear":
                            assert body == {"action": "clear", "revision": revision, "approve_profile": True}
                            revision += 1
                            view = {"revision": revision, "state": "unconfigured", "active": False, "available": True, "read_enabled": False, "read_scope": "home", "playback_enabled": False, "audio_enabled": False, "visual_enabled": False, "comments_enabled": False, "likes_enabled": False, "replies_enabled": False, "publish_enabled": False}
                        elif action == "cancel":
                            assert body == {"action": "cancel", "revision": revision, "attempt_id": view["attempt_id"]}
                            revision += 1
                            view = {"revision": revision, "state": "unconfigured", "active": False, "available": True, "read_enabled": False, "read_scope": "home", "playback_enabled": False, "audio_enabled": False, "visual_enabled": False, "comments_enabled": False, "likes_enabled": False, "replies_enabled": False, "publish_enabled": False}
                        else:
                            raise AssertionError(f"unexpected web-login action: {action}")
                        fulfill_json(view)(route)

                    page.route("**/api/local/apps/douyin/web-login", web_login)
                    page.goto(origin)
                    page.locator("#login-password").fill("synthetic-home-owner")
                    page.locator("#login-submit").click()
                    page.locator("#home-view").wait_for(state="visible")
                    if page.locator("#initialize-button").is_visible(timeout=500):
                        page.locator("#initialize-button").click()
                        page.locator("#initialize-button").wait_for(state="hidden")
                    home_tab(page, "apps")
                    page.locator("#apps-list[aria-busy='false']").wait_for()
                    open_application(page, "抖音")
                    page.get_by_text("尚未保存网页登录资料", exact=True).wait_for()
                    assert page.get_by_text("API 接入（可选）", exact=True).count() == 0
                    assert page.get_by_label("抖音 Client Key", exact=True).count() == 0
                    page.get_by_role("button", name="接入版本管理", exact=True).click()
                    page.get_by_text("接入版本", exact=True).wait_for()
                    assert page.get_by_text("源码", exact=False).count() == 0
                    page.screenshot(path=str(output / f"douyin-integration-version-{suffix}.png"), full_page=True)
                    page.get_by_role("button", name="账号与使用", exact=True).click()
                    assert page.get_by_role("button", name="允许读取首页文字", exact=True).count() == 0
                    assert page.get_by_label("读取范围", exact=True).count() == 0
                    assert page.get_by_label("允许静音观察视频播放", exact=True).count() == 0
                    assert page.get_by_label("允许本机转写视频音轨", exact=True).count() == 0
                    assert page.get_by_label("允许本机分析视频画面", exact=True).count() == 0
                    assert page.get_by_label("允许读取视频评论", exact=True).count() == 0
                    assert page.get_by_text("账号已验证", exact=True).count() == 0
                    assert page.get_by_text("已取得额外 API 权限", exact=False).count() == 0
                    page.get_by_role("button", name="打开网页登录窗口", exact=True).click()
                    page.get_by_text("网页登录窗口已打开", exact=True).wait_for()
                    page.get_by_role("button", name="我已完成登录，保存", exact=True).wait_for()
                    # The next ordinary polling response may only move to an unconfirmed state.
                    view = {"revision": revision, "state": "unconfirmed", "active": False, "available": True, "attempt_id": "attempt-1", "read_enabled": False, "read_scope": "home"}
                    page.locator("#app-dialog-close").click()
                    open_application(page, "抖音")
                    page.get_by_text("等待主人确认", exact=True).wait_for()
                    assert page.get_by_role("button", name="允许读取首页文字", exact=True).count() == 0
                    assert page.get_by_label("读取范围", exact=True).count() == 0
                    assert page.get_by_label("允许本机分析视频画面", exact=True).count() == 0
                    page.get_by_role("button", name="我已完成登录，保存", exact=True).click()
                    page.get_by_text("网页登录资料已保存", exact=True).wait_for()
                    page.get_by_text("最近主人确认：", exact=False).wait_for()
                    assert page.get_by_text("登录状态以网页为准，保存后可用于本软件已支持的抖音功能。", exact=True).count() == 1
                    page.get_by_text("普通账号发布许可未开启。", exact=True).wait_for()
                    assert page.get_by_role("button", name="登录创作者中心", exact=True).is_disabled()
                    page.get_by_role("checkbox", name="允许普通账号发布", exact=True).check()
                    page.get_by_text("已允许普通账号发布。", exact=True).wait_for()
                    page.get_by_text("已保存普通账号发布许可。", exact=True).wait_for()
                    assert view["publish_enabled"] is True and view["read_enabled"] is False, "发布许可不能悄悄开启网页文字读取"
                    assert page.get_by_role("button", name="登录创作者中心", exact=True).is_enabled()
                    page.get_by_role("button", name="允许读取首页文字", exact=True).click()
                    page.get_by_role("button", name="停止网页读取", exact=True).wait_for()
                    page.get_by_text("已允许读取实际网页文字。", exact=False).wait_for()
                    scope = page.get_by_label("读取范围", exact=True)
                    assert scope.input_value() == "home"
                    assert page.get_by_label("允许静音观察视频播放", exact=True).count() == 0
                    assert page.get_by_label("允许本机转写视频音轨", exact=True).count() == 0
                    assert page.get_by_label("允许本机分析视频画面", exact=True).count() == 0
                    scope.select_option("home_and_videos")
                    page.get_by_text("视频页文字不等于音轨或完整观看", exact=False).wait_for()
                    assert scope.input_value() == "home_and_videos"
                    assert not authorization_requests, "ordinary Douyin login must not request the removed API authorization path"
                    # Older wide-text responses have no media flags; reopening must not grant either.
                    view.pop("playback_enabled", None)
                    view.pop("audio_enabled", None)
                    view.pop("visual_enabled", None)
                    view["visual_ready"] = False
                    view["visual_reason"] = "本机视觉工具尚未就绪，请先启用并配置媒体理解。"
                    page.locator("#app-dialog-close").click()
                    open_application(page, "抖音")
                    playback = page.get_by_label("允许静音观察视频播放", exact=True)
                    playback.wait_for()
                    assert not playback.is_checked(), "扩大文字范围不得自动开启播放许可；旧状态缺省关闭"
                    assert "allow_playback" not in requests[-1]
                    assert "allow_audio" not in requests[-1]
                    audio = page.get_by_label("允许本机转写视频音轨", exact=True)
                    assert not audio.is_checked(), "旧状态缺省关闭音轨许可，扩大文字范围不能自动打开"
                    visual = page.get_by_label("允许本机分析视频画面", exact=True)
                    assert not visual.is_checked(), "旧状态缺省关闭画面分析许可，扩大文字范围不能自动打开"
                    comments = page.get_by_label("允许读取视频评论", exact=True)
                    assert not comments.is_checked(), "扩大网页读取范围不能自动打开评论读取"
                    assert not comments.is_disabled(), "网页可用时主人可单独开启评论读取"
                    likes = page.get_by_label("允许点赞", exact=True)
                    replies = page.get_by_label("允许回复评论", exact=True)
                    assert not likes.is_checked(), "扩大网页读取范围不能自动打开点赞"
                    assert not replies.is_checked() and replies.is_disabled(), "回复评论必须先取得评论读取许可"
                    assert visual.is_disabled(), "视觉工具未就绪时不能误启用画面分析"
                    page.get_by_text("本机视觉工具尚未就绪，请先启用并配置媒体理解。", exact=False).wait_for()
                    page.locator("#app-dialog").screenshot(path=str(output / f"douyin-web-login-visual-unavailable-{suffix}.png"))
                    visual_ready = True
                    view["visual_ready"] = True
                    view.pop("visual_reason", None)
                    page.locator("#app-dialog-close").click()
                    open_application(page, "抖音")
                    visual = page.get_by_label("允许本机分析视频画面", exact=True)
                    assert not visual.is_disabled(), "视觉工具就绪后应允许主人单独开启画面分析"
                    visual.check()
                    page.get_by_text("已允许使用本机视觉分析视频画面。", exact=True).wait_for()
                    assert visual.is_checked() and not playback.is_checked() and not audio.is_checked(), "画面分析许可不能开启播放或音轨转写"
                    page.get_by_text("交给媒体理解中配置的视觉服务处理", exact=False).wait_for()
                    page.locator("#app-dialog").screenshot(path=str(output / f"douyin-web-login-visual-enabled-{suffix}.png"))
                    visual.uncheck()
                    page.get_by_text("已停止视频画面分析。", exact=True).wait_for()
                    assert not visual.is_checked(), "主人撤销画面分析后必须立即反映"
                    visual.check()
                    page.get_by_text("已允许使用本机视觉分析视频画面。", exact=True).wait_for()
                    previous_revision = revision
                    audio.click()
                    page.get_by_text("本机语音识别工具未就绪，请先启用并配置识别模型。", exact=True).first.wait_for()
                    page.locator(".douyin-web-audio-label input:not(:disabled)").wait_for()
                    assert page.get_by_text("本机语音识别工具未就绪，请先启用并配置识别模型。", exact=True).count() == 1, "同一故障只在控件旁展示一次"
                    assert revision == previous_revision and not audio.is_checked(), "识别工具未就绪时不能保存许可"
                    assert not playback.is_checked()
                    page.locator("#app-dialog").screenshot(path=str(output / f"douyin-web-login-audio-unavailable-{suffix}.png"))
                    asr_ready = True
                    audio.check()
                    page.get_by_text("已允许本机转写视频音轨。", exact=True).wait_for()
                    assert audio.is_checked() and not playback.is_checked(), "可以只开转写，不开播放观察"
                    playback.check()
                    page.get_by_text("已允许静音观察视频播放。", exact=True).wait_for()
                    assert view["playback_enabled"] is True
                    assert audio.is_checked(), "开启播放观察不能覆盖转写许可"
                    # Saving a permission must lock the entire control until the
                    # server confirms it; this prevents a second click from
                    # overwriting another independently saved media grant.
                    page.evaluate("window.fixtureDelayWebLogin = true")
                    comments.evaluate("node => node.click()")
                    page.get_by_label("允许读取视频评论", exact=True).wait_for(state="attached")
                    assert page.get_by_label("允许读取视频评论", exact=True).is_disabled(), "保存评论读取许可时必须禁用控件"
                    page.evaluate("window.fixtureDelayWebLogin = false")
                    page.get_by_text("已允许读取视频评论。", exact=True).wait_for()
                    assert comments.is_checked() and audio.is_checked() and playback.is_checked(), "评论读取不得覆盖现有媒体许可"
                    likes = page.get_by_label("允许点赞", exact=True)
                    replies = page.get_by_label("允许回复评论", exact=True)
                    assert not replies.is_disabled(), "评论读取保存后应允许主人单独开启回复评论"
                    page.evaluate("window.fixtureDelayWebLogin = true")
                    likes.evaluate("node => node.click()")
                    page.get_by_label("允许点赞", exact=True).wait_for(state="attached")
                    assert page.get_by_label("允许点赞", exact=True).is_disabled(), "保存点赞许可时必须禁用控件"
                    assert page.get_by_label("允许回复评论", exact=True).is_disabled(), "保存点赞许可时必须同时禁用相关互动控件"
                    page.evaluate("window.fixtureDelayWebLogin = false")
                    page.get_by_text("已允许点赞。", exact=True).wait_for()
                    assert view["likes_enabled"] is True and view["replies_enabled"] is False, "点赞保存不能覆盖回复许可"
                    page.get_by_label("允许回复评论", exact=True).check()
                    page.get_by_text("已允许回复视频评论。", exact=True).wait_for()
                    assert view["likes_enabled"] is True and view["replies_enabled"] is True, "回复许可必须独立保存"
                    replies.scroll_into_view_if_needed()
                    assert_no_horizontal_overflow(page, f"{width}-pixel Douyin interaction permissions")
                    page.locator("#app-dialog").screenshot(path=str(output / f"douyin-web-interactions-{suffix}.png"))
                    audio.uncheck()
                    page.get_by_text("已停止视频音轨转写。", exact=True).wait_for()
                    assert playback.is_checked(), "撤销转写不能撤销播放观察"
                    audio.check()
                    page.get_by_text("已允许本机转写视频音轨。", exact=True).wait_for()
                    playback.uncheck()
                    page.get_by_text("已停止静音播放观察。", exact=True).wait_for()
                    assert view["playback_enabled"] is False
                    assert audio.is_checked(), "撤销播放观察不能撤销转写"
                    before_gets = get_count
                    playback_conflict = True
                    # A failed save intentionally restores unchecked state; use one click rather than
                    # check(), which may retry a click while the controlled checkbox is replaced.
                    playback.click()
                    page.get_by_text("播放许可已在别处更新，请重新读取。", exact=True).first.wait_for()
                    page.locator(".douyin-web-playback-label input:not(:disabled)").wait_for()
                    assert get_count > before_gets, "保存冲突后应重新读取服务端许可"
                    assert not playback.is_checked(), "冲突不得把未保存的播放许可留在界面上"
                    playback.check()
                    page.get_by_text("已允许静音观察视频播放。", exact=True).wait_for()
                    scope.select_option("home")
                    page.get_by_text("已允许读取抖音首页网页文字。", exact=True).wait_for()
                    assert view["playback_enabled"] is False
                    assert view["audio_enabled"] is False
                    assert view["visual_enabled"] is False
                    assert view["comments_enabled"] is False
                    assert view["likes_enabled"] is False
                    assert view["replies_enabled"] is False
                    assert playback.count() == 0, "降级为仅首页时应移除播放许可控件"
                    assert audio.count() == 0, "降级为仅首页时应移除音轨许可控件"
                    assert visual.count() == 0, "降级为仅首页时应移除画面分析许可控件"
                    assert comments.count() == 0, "降级为仅首页时应移除评论读取许可控件"
                    scope.select_option("home_and_videos")
                    playback.wait_for()
                    assert not playback.is_checked()
                    assert not audio.is_checked()
                    visual = page.get_by_label("允许本机分析视频画面", exact=True)
                    assert not visual.is_checked()
                    audio.check()
                    page.get_by_text("已允许本机转写视频音轨。", exact=True).wait_for()
                    playback.check()
                    page.get_by_text("已允许静音观察视频播放。", exact=True).wait_for()
                    page.get_by_role("button", name="停止网页读取", exact=True).click()
                    page.get_by_role("button", name="允许读取首页文字", exact=True).wait_for()
                    assert view["playback_enabled"] is False and playback.count() == 0
                    assert view["audio_enabled"] is False and audio.count() == 0
                    assert view["visual_enabled"] is False and visual.count() == 0
                    page.get_by_role("button", name="允许读取首页文字", exact=True).click()
                    page.get_by_label("读取范围", exact=True).wait_for()
                    scope.select_option("home_and_videos")
                    playback.wait_for()
                    assert not audio.is_checked()
                    visual = page.get_by_label("允许本机分析视频画面", exact=True)
                    assert not visual.is_checked()
                    audio.check()
                    page.get_by_text("已允许本机转写视频音轨。", exact=True).wait_for()
                    playback.check()
                    page.get_by_text("已允许静音观察视频播放。", exact=True).wait_for()
                    assert_no_horizontal_overflow(page, f"{width}-pixel Douyin playback permission")
                    assert page.locator("#app-dialog").evaluate("node => node.scrollTop") == 0, "应用弹窗外层不能随内部操作滚动并裁切标题"
                    page.locator("#app-dialog").screenshot(path=str(output / f"douyin-web-login-playback-{suffix}.png"))
                    page.locator("#app-dialog").screenshot(path=str(output / f"douyin-web-login-audio-{suffix}.png"))
                    before_gets = get_count
                    scope_conflict = True
                    scope.select_option("home")
                    page.get_by_text("读取范围已在别处更新，请重新读取。", exact=True).first.wait_for()
                    page.locator(".douyin-web-playback-label input:not(:disabled)").wait_for()
                    assert get_count > before_gets
                    assert page.get_by_label("读取范围", exact=True).input_value() == "home_and_videos", "失败后必须保留服务端读取范围"
                    assert playback.is_checked()
                    assert audio.is_checked()
                    page.locator("#app-dialog").screenshot(path=str(output / f"douyin-web-login-read-{suffix}.png"))
                    # Re-entering Creator Center starts a fresh login and revokes its old publish grant.
                    page.get_by_role("button", name="登录创作者中心", exact=True).click()
                    page.get_by_text("网页登录窗口已打开", exact=True).wait_for()
                    assert view["read_enabled"] is False, "重新登录必须撤销上一账号的首页文字读取许可"
                    assert view["read_scope"] == "home", "重新登录必须恢复默认读取范围"
                    assert view["playback_enabled"] is False, "重新登录必须撤销播放许可"
                    assert view["audio_enabled"] is False, "重新登录必须撤销音轨许可"
                    assert view["visual_enabled"] is False, "重新登录必须撤销画面分析许可"
                    assert view["likes_enabled"] is False, "重新登录必须撤销点赞许可"
                    assert view["replies_enabled"] is False, "重新登录必须撤销回复评论许可"
                    assert view["publish_enabled"] is False, "重新登录必须撤销普通账号发布许可"
                    assert playback.count() == 0
                    assert audio.count() == 0
                    assert visual.count() == 0
                    view = {"revision": revision, "state": "unconfirmed", "active": False, "available": True, "attempt_id": "attempt-2", "read_enabled": False, "read_scope": "home"}
                    page.locator("#app-dialog-close").click()
                    open_application(page, "抖音")
                    page.get_by_text("等待主人确认", exact=True).wait_for()
                    # Removing old profile data stays available even after the browser window is gone.
                    page.get_by_role("button", name="移除网页登录资料", exact=True).click()
                    page.get_by_text("尚未保存网页登录资料", exact=True).wait_for()

                    # A new attempt gets a new opaque identifier; cancelling uses that exact identifier.
                    page.get_by_role("button", name="打开网页登录窗口", exact=True).click()
                    page.get_by_role("button", name="取消本次登录", exact=True).click()
                    page.get_by_text("尚未保存网页登录资料", exact=True).wait_for()
                    assert len(requests) == 33, f"unexpected web-login request count: {len(requests)}"
                    assert not dialogs, "网页登录操作不得弹出浏览器原生确认框"
                    assert_no_horizontal_overflow(page, f"{width}-pixel Douyin web login")
                    page.locator("#app-dialog").screenshot(path=str(output / f"douyin-web-login-{suffix}.png"))

                    # A delayed 401 from a closed detail must not log the owner out or repaint stale state.
                    page.locator("#app-dialog-close").click()
                    late_401 = True
                    page.evaluate("window.fixtureDelayWebLogin = true")
                    page.get_by_role("button", name="管理抖音", exact=True).click()
                    page.locator("#app-dialog").wait_for(state="visible")
                    page.locator("#app-dialog-close").click()
                    page.evaluate("window.fixtureDelayWebLogin = false")
                    page.get_by_role("button", name="管理抖音", exact=True).click()
                    page.locator("#app-dialog").wait_for(state="visible")
                    page.wait_for_timeout(1100)
                    assert page.locator("#home-view").is_visible(), "late closed-detail 401 must not end the current session"
                    # Logging out also must not let a pending UI poll repopulate the old detail.
                    page.locator("#app-dialog-close").click()
                    page.locator("#local-logout-button").click()
                    page.locator("#login-view").wait_for(state="visible")
                    assert not page.locator("#app-dialog").evaluate("node => node.open")
                    context.close()
                browser.close()
        finally:
            stop(process)
    print("Douyin ordinary web-login UI replay passed (synthetic local API only, no platform requests).")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    verify(args.binary.resolve(), args.output.resolve())
