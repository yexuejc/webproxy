// 用 CDP (Chrome DevTools Protocol) 真实启动 headless Chrome 验证 UI 页面 JS 是否正常
const http = require('http');
const WS = globalThis.WebSocket;

function getJSON(path) {
  return new Promise((res, rej) => {
    http.get('http://127.0.0.1:9222' + path, r => {
      let d = ''; r.on('data', c => d += c); r.on('end', () => res(JSON.parse(d)));
    }).on('error', rej);
  });
}

(async () => {
  const targets = await getJSON('/json');
  const target = targets.find(t => t.type === 'page') || targets[0];
  const ws = new WS(target.webSocketDebuggerUrl);
  let msgId = 0; const pending = {}; const exceptions = [];
  function send(method, params = {}) {
    return new Promise((res) => {
      const id = ++msgId; pending[id] = res;
      ws.send(JSON.stringify({ id, method, params }));
    });
  }
  ws.addEventListener('message', ev => {
    const m = JSON.parse(ev.data);
    if (m.id && pending[m.id]) { pending[m.id](m); delete pending[m.id]; }
    if (m.method === 'Runtime.exceptionThrown') {
      exceptions.push(m.params.exceptionDetails);
    }
  });
  await new Promise(r => { if (ws.readyState === 1) return r(); ws.addEventListener('open', r); });
  await send('Runtime.enable');
  await send('Page.enable');
  await send('Page.navigate', { url: 'http://127.0.0.1:8765/' });
  await new Promise(r => setTimeout(r, 2500)); // 等页面加载 + 脚本执行

  const evalRes = await send('Runtime.evaluate', {
    expression: "try{document.getElementById('url').value='http://example.com/';go();document.getElementById('view').src}catch(e){'ERR:'+e.message}"
  });
  const src = evalRes.result && evalRes.result.result && evalRes.result.result.value;
  const expected = 'http://127.0.0.1:8765/p/aHR0cDovL2V4YW1wbGUuY29tLw';

  console.log('=== 真实浏览器(headless Chrome)验证结果 ===');
  console.log('页面 JS 异常数:', exceptions.length);
  exceptions.forEach((e, i) => console.log('  异常' + i + ':', (e.exception && e.exception.description) || e.text));
  console.log('go() 生成的 iframe.src :', src);
  console.log('后端期望的代理地址     :', expected);
  console.log('是否匹配:', src === expected);
  ws.close();
  process.exit(0);
})().catch(e => { console.error('SCRIPT ERR', e); process.exit(1); });
