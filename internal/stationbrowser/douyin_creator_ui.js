(input) => {
  const visible = e => e && e.getClientRects().length > 0 && getComputedStyle(e).visibility !== "hidden" && getComputedStyle(e).display !== "none";
  const locationOK = location.origin === input.origin && location.pathname.startsWith("/creator-micro/");
  if (!locationOK) return { invalid: true };
  const visibleLogin = [...document.querySelectorAll('input[type=password],form[action*="login"],iframe[src*="captcha"],iframe[src*="verify"]')].some(visible);
  if (visibleLogin || /\/login(?:\/|$)/u.test(location.pathname) ||
    [...document.querySelectorAll('h1,h2,h3')].some(e => visible(e) && /(?:扫码登录|登录创作者中心)/u.test(e.textContent))) return { login: true };
  if (input.action === "query") {
    if (!location.pathname.startsWith("/creator-micro/content/manage")) return { invalid: true };
    const matches = [...document.querySelectorAll("a[href]")].filter(visible).filter(e => { try {
      const url = new URL(e.href, location.href);
      return url.origin === "https://www.douyin.com" && url.pathname === "/video/" + input.remoteID && !url.username && !url.password;
    } catch { return false; }});
    // A work still under moderation is accepted, not proven published. An
    // unrelated page-wide success toast is never evidence for this work.
    const published = matches.some(e => {
      const card = e.closest('article,[data-e2e="work-item"],[data-e2e="video-card"]');
      return !!card && /(?:^|\s)(?:已发布|发布成功)(?:\s|$)/u.test(card.innerText);
    });
    return { listed: matches.length > 0, published };
  }
  const files = [...document.querySelectorAll('input[type=file]')].filter(e => /video|mp4|webm/i.test(e.accept));
  const savedTitle = globalThis.__ahsCreatorTitle;
  const titles = savedTitle?.isConnected && visible(savedTitle)
    ? [savedTitle] : [...document.querySelectorAll('input[placeholder*="标题"],input[aria-label*="标题"]')].filter(visible);
  const savedEditor = globalThis.__ahsCreatorEditor;
  const editors = savedEditor?.isConnected && visible(savedEditor) && (savedEditor.isContentEditable || savedEditor instanceof HTMLTextAreaElement)
    ? [savedEditor]
    : [...document.querySelectorAll('[contenteditable=true][data-placeholder*="描述"],[contenteditable=true][data-placeholder*="简介"],[contenteditable=true][role=textbox],[contenteditable=true].editor-kit-container,textarea[placeholder*="描述"],textarea[placeholder*="简介"]')].filter(visible);
  const publishButtons = [...document.querySelectorAll('button,[role=button]')].filter(e => visible(e) && e.textContent.trim() === "发布");
  const readText = e => e && (e instanceof HTMLInputElement || e instanceof HTMLTextAreaElement) ? e.value : (e?.innerText || e?.textContent || "");
  const comparable = text => String(text).replace(/[\u200B\uFEFF]/gu, "").replace(/\r\n/gu,"\n").trim();
  const setText = (e, text) => {
    if (e instanceof HTMLInputElement || e instanceof HTMLTextAreaElement) {
      const owner = e instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
      Object.getOwnPropertyDescriptor(owner, "value").set.call(e, text);
    } else {
      e.focus();
      const selection = getSelection(), range = document.createRange();
      range.selectNodeContents(e);
      selection.removeAllRanges(); selection.addRange(range);
      if (!document.execCommand("insertText", false, text)) return false;
    }
    e.dispatchEvent(new InputEvent("input", { bubbles: true, inputType: "insertText", data: text }));
    e.dispatchEvent(new Event("change", { bubbles: true }));
    return true;
  };
  const caption = [input.title, input.body].filter(Boolean).join("\n");
  if (input.action === "bind-input") {
    if (files.length !== 1) return { invalid: true };
    globalThis.__ahsCreatorInput = files[0];
    return { selected: true };
  }
  if (input.action === "pin-file") {
    const node = globalThis.__ahsCreatorInput;
    if (!node || node.files.length !== 1) return { invalid: true };
    const file = node.files[0];
    if (file.size <= 0 || file.size !== input.mediaBytes) return { invalid: true };
    globalThis.__ahsCreatorFile = { node, file, name: file.name, size: file.size };
    return { selected: true };
  }
  if (input.action === "fill") {
    if (titles.length > 1 || editors.length !== 1 || (titles.length === 1 && titles[0].maxLength > 0 && input.title.length > titles[0].maxLength)) return { invalid: true };
    if (titles.length === 1) globalThis.__ahsCreatorTitle = titles[0];
    if (titles.length === 1 && !setText(titles[0], input.title)) return { invalid: true };
    globalThis.__ahsCreatorEditor = editors[0];
    if (!setText(editors[0], titles.length === 1 ? input.body : caption)) return { invalid: true };
  }
  const wanted = { public: /^(公开|所有人可见)$/, private: /^(私密|仅自己可见|仅自己)$/, friends: /^(好友可见|互关朋友可见)$/ }[input.visibility];
  const choices = [...document.querySelectorAll('label,[role=radio]')].filter(e => visible(e) && wanted?.test(e.textContent.replace(/\s+/g, "").trim()));
  const selected = e => e.getAttribute("aria-checked") === "true" || e.getAttribute("data-checked") === "true" ||
    e.querySelector('input[type=radio],input[type=checkbox]')?.checked === true || (e instanceof HTMLInputElement && e.checked);
  if (input.action === "fill" && choices.length === 1 && !selected(choices[0])) choices[0].click();
  const visibilityOK = choices.length === 1 && selected(choices[0]);
  const filled = editors.length === 1 && comparable(readText(editors[0])) === comparable(titles.length === 1 ? input.body : caption) &&
    (titles.length === 0 || comparable(readText(titles[0])) === comparable(input.title));
  const button = publishButtons.length === 1 ? publishButtons[0] : null;
  const enabled = !!button && !button.disabled && button.getAttribute("aria-disabled") !== "true";
  const pin = globalThis.__ahsCreatorFile;
  const previews = [...document.querySelectorAll('video')].filter(e => visible(e) && e.readyState >= 2 && Number.isFinite(e.duration) && e.duration > 0);
  const uploadFailed = /上传失败|重新上传失败/u.test(document.body?.innerText || "");
  // The creator SPA legitimately removes its upload input after processing.
  // Retain the original native File in our isolated world, permit that one
  // transition only with its resulting unique playable preview, and reject
  // cleared/replaced connected inputs or a newly selected second video.
  const attachedOriginal = !!pin && pin.node.isConnected && pin.node.files.length === 1 &&
    pin.node.files[0] === pin.file && pin.file.size === input.mediaBytes;
  const completedUploadRoute = location.pathname === "/creator-micro/content/post/video";
  const uploadInputReset = !!pin && (!pin.node.isConnected || (completedUploadRoute && pin.node.files.length === 0));
  const uploadedOriginal = !!pin && uploadInputReset && pin.file.size === input.mediaBytes && previews.length === 1 &&
    !uploadFailed && !files.some(e => e.files.length > 0);
  const fileLost = !!pin && ((pin.node.isConnected && !attachedOriginal && !completedUploadRoute) ||
    (!pin.node.isConnected && files.some(e => e.files.length > 0)));
  const fileStillBound = !!pin && pin.size > 0 && (attachedOriginal || uploadedOriginal);
  if (uploadFailed) return { uploadFailed: true };
  if (input.action === "submit") {
    if (!filled || !visibilityOK || !enabled || !fileStillBound) return { invalid: true };
    button.click();
    return { clicked: true };
  }
  return { fileAvailable: files.length === 1, editorAvailable: editors.length === 1 && titles.length <= 1,
    filled, visibilityOK, enabled, fileStillBound, fileLost, previewCount: previews.length, pinMatches: !!pin && pin.file.size === input.mediaBytes,
    previewState: [...document.querySelectorAll('video')].map(e=>[e.readyState,String(e.duration),e.error?.code||0,e.getClientRects().length].join(':')).join(',') };
}
