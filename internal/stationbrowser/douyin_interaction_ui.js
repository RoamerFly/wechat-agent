(input) => {
  // This small page adapter intentionally accepts no selector from callers.
  // Every write has an exact rendered target and may fail closed when Douyin
  // changes its public UI.
  const visible = e => !!e && e.getClientRects().length > 0 && getComputedStyle(e).display !== "none" && getComputedStyle(e).visibility !== "hidden";
  const locationOK = (() => { try { const want = new URL(input.target); return location.origin === want.origin && location.pathname === want.pathname && location.search === want.search && location.hash === want.hash; } catch { return false; } })();
  if (!locationOK) return { invalid: true };
  const login = [...document.querySelectorAll('input[type=password],form[action*="login"],iframe[src*="captcha"],iframe[src*="verify"]')].some(visible) || /\/login(?:\/|$)/u.test(location.pathname);
  if (login) return { login: true };
  const cardFor = id => {
    const matches = [...document.querySelectorAll('[data-e2e="comment-item"][data-comment-id], [data-comment-id][data-e2e]')].filter(visible).filter(e => e.getAttribute('data-comment-id') === input.commentID);
    return matches.length === 1 ? matches[0] : null;
  };
  const ownReply = () => {
    const baseline = globalThis.__ahsReplyBaseline || new Set();
    const nodes = [...document.querySelectorAll('[data-e2e="comment-reply-item"][data-reply-id], [data-reply-id][data-parent-comment-id]')].filter(visible).filter(e =>
      (!input.remoteID ? !baseline.has(e.getAttribute('data-reply-id')) : e.getAttribute('data-reply-id') === input.remoteID) && e.getAttribute('data-parent-comment-id') === input.commentID &&
      e.getAttribute('data-author-self') === 'true' && (e.querySelector('[data-e2e="comment-content"]')?.textContent || e.textContent || '').trim() === input.text.trim());
    return nodes.length === 1 ? nodes[0] : null;
  };
  if (input.kind === 'like') {
    const controls = [...document.querySelectorAll('[data-e2e="like-button"][aria-pressed], [data-e2e="like-button"][data-liked]')].filter(visible).filter(e => !e.hasAttribute('data-video-id') || e.getAttribute('data-video-id') === input.videoID);
    if (controls.length !== 1) return { ready: true, url: location.href, code: 'like_control_unavailable' };
    const control = controls[0];
    const aria=control.getAttribute('aria-pressed'), liked=control.getAttribute('data-liked');
    if ((aria!==null && aria!=='true' && aria!=='false') || (liked!==null && liked!=='true' && liked!=='false') || (aria!==null && liked!==null && aria!==liked)) return {ready:true,url:location.href,code:'like_state_unavailable'};
    const actual = (aria ?? liked) === 'true';
    if (control.disabled || control.getAttribute('aria-disabled')==='true') return {ready:true,url:location.href,code:'like_control_disabled'};
    if (input.action === 'submit') {
      if (actual === !!input.desired) return { ready: true, actual: true, desired: actual, url: location.href, remoteID: 'like:' + input.videoID };
      control.click();
    }
    const after = control.getAttribute('aria-pressed') === 'true' || control.getAttribute('data-liked') === 'true';
    return { ready: true, actual: after === !!input.desired, desired: after, url: location.href, remoteID: 'like:' + input.videoID };
  }
  const card = cardFor(input.commentID);
  if (!card) return { ready: true, url: location.href, code: 'reply_parent_unavailable' };
  const parent = card.querySelector('[data-e2e="comment-content"]');
  const parentText = (parent?.textContent || '').trim();
  const known = ownReply();
  if (input.action === 'query') return { ready: true, actual: !!known, self: !!known, remoteID: known?.getAttribute('data-reply-id') || '', parentText, url: location.href };
  if (parentText !== input.expectedParentText) return {ready:true,parentText,url:location.href,code:'comment_changed'};
  const getEditors = () => [...document.querySelectorAll('[data-e2e="comment-reply-editor"][data-reply-to] textarea, textarea[data-e2e="comment-reply-editor"][data-reply-to]')].filter(visible).filter(e => (e.getAttribute('data-reply-to')||e.closest('[data-reply-to]')?.getAttribute('data-reply-to')) === input.commentID);
  if (input.action === 'prepare' && getEditors().length === 0) {
    const buttons=[...card.querySelectorAll('[data-e2e="comment-reply-button"]')].filter(visible);
    if(buttons.length!==1)return {ready:true,parentText,url:location.href,code:'reply_control_unavailable'};
    buttons[0].click();
  }
  const editors=getEditors();
  if(editors.length!==1)return {ready:true,parentText,url:location.href,code:'reply_editor_unavailable'};
  const editor=editors[0];
  if(editor.maxLength>0&&input.text.length>editor.maxLength)return {ready:true,parentText,url:location.href,code:'reply_text_limit_'+editor.maxLength};
  if(input.action==='prepare'){
    Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set.call(editor,input.text);
    editor.dispatchEvent(new InputEvent('input',{bubbles:true,inputType:'insertText',data:input.text}));
  }
  if(editor.value!==input.text)return {ready:true,parentText,url:location.href,code:'reply_editor_rejected'};
  const submit=[...document.querySelectorAll('[data-e2e="comment-reply-submit"][data-reply-to]')].filter(visible).filter(e=>e.getAttribute('data-reply-to')===input.commentID&&!e.disabled&&e.getAttribute('aria-disabled')!=='true');
  if(submit.length!==1)return {ready:true,parentText,url:location.href,code:'reply_submit_unavailable'};
  if((cardFor(input.commentID)?.querySelector('[data-e2e=comment-content]')?.textContent||'').trim()!==input.expectedParentText)return {ready:true,parentText,url:location.href,code:'comment_changed'};
  if(input.action==='prepare')return {ready:true,prepared:true,parentText,url:location.href};
  if(input.action!=='submit')return {invalid:true};
  globalThis.__ahsReplyBaseline=new Set([...document.querySelectorAll('[data-reply-id]')].map(e=>e.getAttribute('data-reply-id')));
  submit[0].click();
  return {ready:true,parentText,url:location.href};
}
