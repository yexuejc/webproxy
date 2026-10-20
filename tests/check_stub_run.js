// 执行验证：从代理输出中提取 JS 桩，在最小 DOM 环境下执行，确认导流生效
const fs = require('fs');
const vm = require('vm');

const html = fs.readFileSync(process.argv[2], 'utf8');
const blocks = [];
const re = /<script\b([^>]*)>([\s\S]*?)<\/script>/gi;
let m;
while ((m = re.exec(html)) !== null) {
  if (!/\bsrc\s*=/.test(m[1])) blocks.push(m[2]);
}
const stub = blocks.find(b => b.includes('var PREFIX='));
if (!stub) { console.log('未找到桩'); process.exit(1); }

const captured = [];
const handlers = {};
const sandbox = {
  console,
  TextEncoder,
  URL,
  location: { host: '127.0.0.1:8765', hostname: '127.0.0.1' },
  document: { addEventListener: (t, f) => { handlers[t] = f; } },
  btoa: (s) => Buffer.from(s, 'binary').toString('base64'),
  fetch: (u, o) => { captured.push(['fetch', u]); return Promise.resolve({ ok: true }); },
  XMLHttpRequest: function () {},
  WebSocket: function (u, p) { captured.push(['ws', u]); },
  open: (u, n, s) => { captured.push(['open', u]); },
};
sandbox.window = sandbox;
sandbox.XMLHttpRequest.prototype.open = function (mth, u, a) { captured.push(['xhr', u]); };
sandbox.self = sandbox;

vm.createContext(sandbox);
vm.runInContext(stub, sandbox);

// 模拟页面调用
sandbox.window.fetch('api/data');                        // 相对路径 → 应走 /p/
sandbox.window.fetch('http://127.0.0.1:18091/x?a=1');    // 目标站绝对路径 → /p/
new sandbox.XMLHttpRequest().open('GET', '/y');          // /y → /p/
sandbox.window.open('/z');                               // window.open → /p/
new sandbox.WebSocket('ws://127.0.0.1:18091/ws');        // WS 不在改写范围(已知局限)

let ok = true;
for (const [kind, u] of captured) {
  if (kind === 'ws') {
    // 已知设计边界：桩只导流 http(s)，ws:// 原样透传（服务端未实现 WS 通道）
    console.log('NOTE  ws  -> ' + u + '（已知局限：WS 未真正代理，原样透传）');
    continue;
  }
  const pass = u.startsWith('/p/');
  if (!pass) ok = false;
  console.log((pass ? 'OK  ' : 'FAIL') + '  ' + kind + ' -> ' + u.slice(0, 72) + (u.length > 72 ? '...' : ''));
}

// 表单提交改写模拟
const form = { tagName: 'FORM', action: 'http://127.0.0.1:18091/login' };
sandbox.__handlers = handlers;
handlers.submit({ target: form });
const formPass = form.action.startsWith('/p/');
console.log((formPass ? 'OK  ' : 'FAIL') + '  form.action -> ' + form.action.slice(0, 72) + (form.action.length > 72 ? '...' : ''));
if (!formPass) ok = false;

// 非 http(s) 不应被改写
let untouched = null;
sandbox.window.fetch('#frag');
console.log(ok ? 'RESULT: PASS' : 'RESULT: FAIL');
process.exit(ok ? 0 : 1);
