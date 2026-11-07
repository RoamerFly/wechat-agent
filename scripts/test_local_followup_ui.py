"""Synthetic local-chat follow-up replay: no model, platform, or WeChat calls."""

import argparse
import json
from pathlib import Path
import tempfile

from playwright.sync_api import expect, sync_playwright

from test_local_home_ui import start, stop


def verify(binary, output):
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="synthetic-followup-ui-", dir=output) as directory:
        directory = Path(directory).resolve()
        process, origin = start(binary, directory / "host", directory / "workspace")
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(channel="msedge", headless=True)
                context = browser.new_context(viewport={"width": 1280, "height": 800})
                fixture = {"items": [], "background_pending": False, "busy": False}
                reads, posts, held, errors = [], [], [], []
                hold_next = [False]

                def fulfill(route, payload):
                    route.fulfill(status=200, content_type="application/json", body=json.dumps(payload, ensure_ascii=False))

                def chat(route):
                    if route.request.method == "POST":
                        payload = route.request.post_data_json
                        posts.append(payload)
                        request_id = payload["request_id"]
                        fixture["items"] = [
                            {"id": f"older-{index}", "role": "assistant", "text": "合成历史，用于检查阅读位置。" * 15, "time": f"2026-11-07T00:{index:02d}:00Z"}
                            for index in range(15)
                        ] + [
                            {"id": "user-original", "role": "user", "request_id": request_id, "text": payload["text"], "time": "2026-11-07T01:00:00Z"},
                            {"id": "assistant-initial", "role": "assistant", "request_id": request_id, "text": "合成初始回复：后台任务已登记。", "time": "2026-11-07T01:00:01Z"},
                        ]
                        fixture["background_pending"] = True
                        fulfill(route, {"status": "completed"})
                    else:
                        reads.append(route.request.url)
                        if hold_next[0]:
                            hold_next[0] = False
                            held.append(route)
                        else:
                            fulfill(route, {"items": fixture["items"], "has_more": False, "busy": fixture["busy"], "background_pending": fixture.get("background_pending", False)})

                context.route("**/api/local/chat?*", chat)
                context.route("**/api/local/chat", chat)
                context.route("**/api/local/model", lambda route: fulfill(route, {"key_configured": True, "model": "synthetic-no-model-call"}))
                page = context.new_page()
                page.on("pageerror", lambda error: errors.append(str(error)))
                page.goto(origin)
                page.locator("#login-password").fill("synthetic-home-owner")
                page.locator("#login-submit").click()
                page.locator("#initialize-button").click()
                page.locator("#chat-view").wait_for(state="visible")
                page.locator("#chat-input").fill("合成任务，请后台完成后继续回复")
                page.locator("#chat-send").click()
                expect(page.locator("#chat-progress")).to_have_text("后台任务完成后会继续回复")
                assert len(posts) == 1
                initial_reads = len(reads)
                page.wait_for_timeout(3400)
                assert len(reads) >= initial_reads + 2, "initial assistant reply stopped background polling"

                page.locator("#chat-input").fill("尚未发送的编辑草稿")
                page.locator("#settings-tab").click()
                paused_reads = len(reads)
                page.wait_for_timeout(1900)
                assert len(reads) == paused_reads, "chat polling continued on another page"
                page.locator("#chat-tab").click()
                expect(page.locator("#chat-progress")).to_have_text("后台任务完成后会继续回复")
                assert len(reads) > paused_reads, "returning to chat did not refresh"
                expect(page.locator("#chat-input")).to_have_value("尚未发送的编辑草稿")

                page.evaluate("""() => {
                    window.fixtureHidden = true;
                    Object.defineProperty(document, 'hidden', {configurable: true, get: () => window.fixtureHidden});
                    document.dispatchEvent(new Event('visibilitychange'));
                }""")
                hidden_reads = len(reads)
                page.wait_for_timeout(1900)
                assert len(reads) == hidden_reads, "hidden page continued polling"
                page.evaluate("window.fixtureHidden = false; document.dispatchEvent(new Event('visibilitychange'))")
                page.wait_for_timeout(100)
                assert len(reads) > hidden_reads, "visible chat did not refresh"

                fixture["busy"] = True
                expect(page.locator("#chat-progress")).to_have_text("正在等待回复…", timeout=4000)
                fixture["busy"] = False
                expect(page.locator("#chat-progress")).to_have_text("后台任务完成后会继续回复", timeout=4000)
                page.locator("#chat-stream").evaluate("node => { node.scrollTop = 80; }")
                before_scroll = page.locator("#chat-stream").evaluate("node => node.scrollTop")
                assert before_scroll > 0, "synthetic history did not create a scroll area"
                fixture["items"].append({"id": "assistant-followup", "role": "assistant", "request_id": posts[0]["request_id"], "text": "合成续办回复：后台任务已经完成。", "time": "2026-11-07T01:10:00Z"})
                fixture["background_pending"] = False
                expect(page.locator("#chat-stream").get_by_text("合成续办回复：后台任务已经完成。", exact=True)).to_have_count(1, timeout=4000)
                expect(page.locator("#chat-stream").get_by_text("合成初始回复：后台任务已登记。", exact=True)).to_have_count(1)
                expect(page.locator("#chat-progress")).to_have_text("")
                expect(page.locator("#chat-input")).to_have_value("尚未发送的编辑草稿")
                assert abs(page.locator("#chat-stream").evaluate("node => node.scrollTop") - before_scroll) <= 1, "background follow-up moved the reading position"
                stopped_reads = len(reads)
                page.wait_for_timeout(1900)
                assert len(reads) == stopped_reads, "settled foreground and background kept polling"
                page.screenshot(path=str(output / "followup-ui-completed.png"))

                # An absent flag must act like false for older servers.
                fixture.pop("background_pending")
                page.locator("#settings-tab").click()
                page.locator("#chat-tab").click()
                page.wait_for_timeout(100)
                compatible_reads = len(reads)
                page.wait_for_timeout(1900)
                assert len(reads) == compatible_reads, "missing background flag enabled polling"

                # Hold a previous session's read until after logout; it must not refill chat.
                hold_next[0] = True
                page.locator("#settings-tab").click()
                page.locator("#chat-tab").click()
                page.wait_for_timeout(100)
                assert len(held) == 1
                page.locator("#local-logout-button").click()
                page.locator("#login-view").wait_for(state="visible")
                fulfill(held.pop(), {"items": [{"id": "late-private", "role": "assistant", "text": "旧会话迟到内容"}], "background_pending": True, "busy": True})
                page.wait_for_timeout(100)
                assert "旧会话迟到内容" not in page.locator("#chat-stream").inner_text()
                expect(page.locator("#chat-progress")).to_have_text("")
                assert not errors, errors
                assert len(posts) == 1, "read-only follow-up polling submitted another chat request"
                browser.close()
        finally:
            stop(process)
    print("Local chat follow-up UI replay passed: synthetic HTTP only, no model or platform calls.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    verify(args.binary.resolve(), args.output.resolve())
