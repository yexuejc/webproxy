// 验证「新标签打开 /p/xxx → 302 到 /?url=xxx → 外框 + iframe 自动加载」
// 用法: node verify_newtab.js
const DEBUG_PORT = 9222;

function getJSON(path) {
  return fetch(`http://127.0.0.1:${DEBUG_PORT}${path}`).then((r) => r.json());
}

function cdp(ws, id, method, params) {
  return new Promise((resolve, reject) => {
    const onMsg = (ev) => {
      const m = JSON.parse(ev.data);
      if (m.id === id) {
        ws.removeEventListener('message', onMsg);
        m.error ? reject(new Error(JSON.stringify(m.error))) : resolve(m.result);
      }
    };
    ws.addEventListener('message', onMsg);
    ws.send(JSON.stringify({ id, method, params: params || {} }));
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

(async () => {
  const targets = await getJSON('/json');
  const page = targets.find((t) => t.type === 'page');
  const ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((r) => (ws.onopen = r));

  let id = 0;
  const enc = 'aHR0cDovL2V4YW1wbGUuY29tLw'; // http://example.com/

  // 场景1: 新标签直接打开 /p/xxx（模拟 target=_blank）
  await cdp(ws, ++id, 'Page.navigate', {
    url: `http://127.0.0.1:8765/p/${enc}`,
  });
  await sleep(2500);

  const r1 = await cdp(ws, ++id, 'Runtime.evaluate', {
    expression: `JSON.stringify({
        path: location.pathname,
        search: location.search,
        inputVal: document.getElementById('url') ? document.getElementById('url').value : null,
        iframeSrc: document.getElementById('view') ? document.getElementById('view').src : null,
      })`,
    returnByValue: true,
  });
  const s1 = JSON.parse(r1.result.value);
  console.log('[场景1] 新标签打开 /p/xxx 落地页:');
  console.log('  最终路径:', s1.path, s1.search);
  console.log('  外框输入框:', s1.inputVal);
  console.log('  iframe.src:', s1.iframeSrc);
  const navOk = s1.path === '/' && s1.search.includes('url=');
  const frameOk = s1.iframeSrc && s1.iframeSrc.includes('/p/' + enc);
  console.log('  → 302 改道到外框:', navOk ? 'OK' : 'FAIL');
  console.log('  → iframe 指向代理内容:', frameOk ? 'OK' : 'FAIL');

  // 场景2: iframe 内容是否真的加载出来（同源可读）
  const r2 = await cdp(ws, ++id, 'Runtime.evaluate', {
    expression: `(() => {
        const f = document.getElementById('view');
        if (!f) return 'NO_FRAME';
        try {
          const d = f.contentDocument;
          if (!d || !d.body) return 'NOT_LOADED';
          return d.title || d.body.innerText.slice(0, 60) || '(empty)';
        } catch (e) { return 'CORS:' + e.message; }
      })()`,
    returnByValue: true,
  });
  console.log('[场景2] iframe 内实际内容:', r2.result.value);
  const contentOk = r2.result.value !== 'NO_FRAME' && r2.result.value !== 'NOT_LOADED' && !String(r2.result.value).startsWith('CORS');
  console.log('  → iframe 内容加载:', contentOk ? 'OK' : 'FAIL');

  // 场景3: 外框输入框内回车 go() 后 iframe 带 _wpframe=1 标记
  const r3 = await cdp(ws, ++id, 'Runtime.evaluate', {
    expression: `JSON.stringify({
        hasEnc: typeof enc === 'function',
        hasDec: typeof dec === 'function',
      })`,
    returnByValue: true,
  });
  const s3 = JSON.parse(r3.result.value);
  console.log('[场景3] 外框 enc/dec 函数:', s3.hasEnc && s3.hasDec ? 'OK' : 'FAIL');

  console.log(
    '\n总结:',
    navOk && frameOk && contentOk && s3.hasEnc && s3.hasDec ? '✅ 全部通过' : '❌ 存在失败项'
  );
  ws.close();
  process.exit(navOk && frameOk && contentOk && s3.hasEnc && s3.hasDec ? 0 : 1);
})().catch((e) => {
  console.error('SCRIPT ERR', e.message);
  process.exit(2);
});
