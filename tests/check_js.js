// 检查脚本：提取 HTML 中所有 <script> 块，用 vm 编译验证语法（等价浏览器执行前的解析）
// 用法: node check_js.js <file.html>
const fs = require('fs');
const vm = require('vm');

const file = process.argv[2];
const html = fs.readFileSync(file, 'utf8');

// 提取 <script>...</script>（含 src 的也列出）
const blocks = [];
const re = /<script\b([^>]*)>([\s\S]*?)<\/script>/gi;
let m;
while ((m = re.exec(html)) !== null) {
  blocks.push({ attrs: m[1].trim(), code: m[2] });
}

let fail = 0;
blocks.forEach((b, i) => {
  const isSrc = /\bsrc\s*=/.test(b.attrs);
  const tag = isSrc ? '[外部脚本 ' + (b.attrs.match(/src\s*=\s*["']?([^"'\s>]+)/i) || [])[1] + ']' : '[内联脚本 #' + (i + 1) + ', ' + b.code.length + ' 字符]';
  if (isSrc) { console.log(tag + ' -> 跳过(外部)'); return; }
  try {
    new vm.Script(b.code, { filename: file });
    console.log(tag + ' -> 语法 OK');
  } catch (e) {
    fail++;
    console.log(tag + ' -> 语法错误: ' + e.message);
  }
});

// 结构性断言
const stubWrapped = /<script>\(function\(\)\{var PREFIX=/.test(html);
console.log('桩是否被 <script> 包裹: ' + (stubWrapped ? '是' : '否(缺陷)'));
const tplIntact = html.includes("var TPL_HEAD = '<meta charset=\"utf-8\"></head>';");
console.log('页面原脚本字面量 </head> 是否完好: ' + (tplIntact ? '是' : '否(被注入破坏)'));

// 桩的位置：应紧邻 </head> 之前
const posStub = html.indexOf('(function(){var PREFIX=');
const posHeadClose = html.lastIndexOf('</head>');
console.log('桩位于 </head> 之前: ' + (posStub >= 0 && posStub < posHeadClose ? '是' : '否'));

// body 顶部是否出现裸 JS 文本（未包裹时的污染现象）
const bodyOpen = html.search(/<body[^>]*>/i);
if (bodyOpen >= 0) {
  const seg = html.slice(bodyOpen, bodyOpen + 200);
  console.log('body 顶部是否有裸 JS 文本: ' + (/^[^<]*\(function/.test(seg.replace(/<body[^>]*>/i, '')) || /function\(\)\{var PREFIX/.test(seg) ? '是(缺陷)' : '否'));
}

console.log(fail === 0 ? 'RESULT: PASS' : 'RESULT: FAIL(' + fail + ')');
process.exit(fail === 0 ? 0 : 1);
