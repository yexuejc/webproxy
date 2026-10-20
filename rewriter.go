// 本文件负责"内容改写"：把上游页面里的 URL 改写为指向本代理的形式。
//
// 两类改写对象：
//  1. HTML 文档：解析 DOM 树后改写属性里的 URL，并在 <head> 末尾注入
//     JS 代理桩（拦截 fetch/XHR/WebSocket/window.open/form 等运行时
//     发起的网络请求）；
//  2. CSS 文本（含 <style> 块与 style="" 属性）：改写 url(...) 引用。
//
// 两种代理形态（由调用方传 subdomainMode 区分）：
//   - 子域名模式：同源地址改写成根相对路径（由子域名源解析），
//     异源地址走 basePath 前缀的 base64 路径；
//   - 纯路径模式（?url= 入口）：所有外链统一走 basePath 前缀。
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// encodeURL 把目标 URL 编码为 base64url（RawURLEncoding：无 '=' 填充）。
//
// 作用：把任意 URL 变成可安全放进路径段的字符串。
// 场景：proxyURL 拼 /p/<base64> 前缀路径；buildProxyJS 里的 JS
// 端 enc() 也使用同一编码（自带去 '=' 逻辑，两端必须一致）。
// 入参：
//   - u：原始目标 URL 字符串。
//
// 出参：
//   - string：base64url 编码结果，字符集 [A-Za-z0-9\-_]，无填充。
func encodeURL(u string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(u))
}

// proxyURL 把任意 URL 改写成走本代理的绝对路径形式。
//
// 作用：生成 "<basePath><base64url(URL)>" 形式的代理路径。
// 场景：改写 HTML 属性 / CSS url() 时的最终产出；前端 JS 桩对
// 异源请求也走同样的拼接规则。
// 入参：
//   - raw：原始（通常已 resolveURL 后的绝对）URL。
//
// 出参：
//   - string：代理路径，如 "/p/aHR0cHM6Ly9..."。
func proxyURL(raw string) string {
	return basePath + encodeURL(raw)
}

// resolveURL 以 base 为基准解析相对/绝对引用，返回绝对 URL 字符串。
//
// 作用：把页面里的相对地址（"/x"、"--/y"、"../z"）解析成完整 URL。
// 场景：rewriteHTML/rewriteCSS 改写每个 URL 前的第一步。
// 入参：
//   - base：文档自身的 URL（改写基准）。
//   - v：待解析的属性值，可能是相对路径或绝对 URL。
//
// 出参：
//   - string：解析后的绝对 URL；输入本身无法解析时返回 ""（调用方
//     据此跳过该项）。
func resolveURL(base *url.URL, v string) string {
	ref, err := url.Parse(v)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

// shouldSkip 判断一个 URL 值是否不需要改写。
//
// 作用：过滤"非网络资源"与"已是代理地址"的取值，避免无意义甚至
// 破坏性的改写（如把 javascript: 伪协议也 base64 化）。
// 场景：rewriteHTML / rewriteCSS 对每个候选 URL 先过这个闸门。
// 入参：
//   - v：原始属性值（未 trim）。
//
// 出参：
//   - bool：true 表示跳过不改写。
func shouldSkip(v string) bool {
	// 统一小写并去空白，防止 "JaVaScript:" 这类大小写变体漏判
	low := strings.ToLower(strings.TrimSpace(v))
	switch {
	// 分支：纯片段（页内锚点）——浏览器本地跳转，不发请求
	case strings.HasPrefix(low, "#"):
		return true
	// 分支：javascript: 伪协议——行内脚本，改写即破坏功能
	case strings.HasPrefix(low, "javascript:"):
		return true
	// 分支：data: 内联资源——本身自包含，无代理必要
	case strings.HasPrefix(low, "data:"):
		return true
	// 分支：blob:——页面运行时生成的本地对象 URL
	case strings.HasPrefix(low, "blob:"):
		return true
	// 分支：mailto: / tel:——非 HTTP 通信协议
	case strings.HasPrefix(low, "mailto:"):
		return true
	case strings.HasPrefix(low, "tel:"):
		return true
	// 分支：about:——浏览器内部页（about:blank 等）
	case strings.HasPrefix(low, "about:"):
		return true
	// 分支：chrome:——浏览器内部页
	case strings.HasPrefix(low, "chrome:"):
		return true
	// 分支：filesystem:——浏览器沙箱文件系统
	case strings.HasPrefix(low, "filesystem:"):
		return true
	// 分支：已是本代理的地址（幂等保护，防止二次改写套娃）
	case strings.HasPrefix(low, basePath):
		return true
	}
	// 其余一律进入改写流程
	return false
}

// urlAttrs 记录"取值是 URL"的 HTML 属性集合。
//
// 作用：rewriteHTML 只改这些属性的值，其余属性一律不碰。
// 场景：DOM 遍历时按 Key 查表决定是否改写。
//
// 条目说明（按用途归类）：
//   - href/src/action：最常见的资源与导航属性（a/link/script/img/form）；
//   - data/poster：<object data>、<video poster>；
//   - cite/background/longdesc：引用/背景/长描述，兼容老站点；
//   - formaction：HTML5 按钮级提交地址，优先级高于 form 的 action；
//   - xlink:href：SVG 资源的引用属性（不带 xmlns 命名空间前缀的
//     简写形式，Go 解析器产出如此）；
//   - lowsrc：远古 IE 时代的低清图属性，老站点仍可能出现。
var urlAttrs = map[string]bool{
	"href": true, "src": true, "action": true, "data": true,
	"poster": true, "cite": true, "background": true,
	"formaction": true, "longdesc": true, "xlink:href": true, "lowsrc": true,
}

// cssURLRe 匹配 CSS 里的 url(...) 片段（含引号与空白）。
//
// 用非贪婪 [^)]*? 而非 .*：防止 url() 嵌套或同行后续内容被吞；
// (?i) 保证 URL( / url( 大小写形态都能匹配。
var cssURLRe = regexp.MustCompile(`(?i)url\(\s*[^)]*?\)`)

// rewriteCSS 改写一段 CSS 文本中所有 url(...) 引用。
//
// 作用：让样式表引用的背景图/字体等也走代理，避免原样直连时被
// 防盗链或跨域策略挡下。
// 场景：rewriteHTML 对 <style> 文本块与 style="" 属性调用。
// 入参：
//   - css：原始 CSS 文本。
//   - base：样式表所在文档的 URL（相对路径解析基准）。
//   - subdomainMode：是否子域名模式（决定同源地址改根相对还是走 /p/）。
//
// 出参：
//   - string：改写后的 CSS 文本；无法识别的片段原样保留。
func rewriteCSS(css string, base *url.URL, subdomainMode bool) string {
	return cssURLRe.ReplaceAllStringFunc(css, func(m string) string {
		// m 形如 `url(...)`（可能含引号/空白）：先剥掉 url( 与 )
		// 两层外壳（4 = len("url(")，末字符为 ")）
		inner := m[4 : len(m)-1]
		inner = strings.TrimSpace(inner)
		// 去掉可选引号（url("x") / url('x') / url(x) 三种形态）
		inner = strings.Trim(inner, "\"'")
		// 分支：空值或命中跳过规则（#/data:/已是代理路径等）——原样返回
		if inner == "" || shouldSkip(inner) {
			return m
		}
		// 以文档 URL 为基准解析为绝对地址
		abs := resolveURL(base, inner)
		// 分支：解析失败——保持原样
		if abs == "" {
			return m
		}
		u, e := url.Parse(abs)
		// 分支：二次解析异常——保持原样
		if e != nil {
			return m
		}
		// 分支：非 http/https 协议（ftp/file 等）——代理不支持，保持原样
		if u.Scheme != "http" && u.Scheme != "https" {
			return m
		}
		// 分支：子域名模式且引用同源——改写成根相对路径（去掉 origin），
		// 让请求落到当前子域名源上，由子域名处理器映射回目标。
		if subdomainMode && u.Host == base.Host {
			rel := u.Path
			// 分支：无路径时用 "/"（代表站点根）
			if rel == "" {
				rel = "/"
			}
			// 查询串无损保留
			if u.RawQuery != "" {
				rel += "?" + u.RawQuery
			}
			return "url(\"" + rel + "\")"
		}
		// 默认：异源地址走 basePath 前缀的 base64 路径。
		// 统一输出双引号形态，避免与页面既有单引号风格冲突。
		return "url(\"" + proxyURL(abs) + "\")"
	})
}

// rewriteHTML 解析并改写整份 HTML 文档，返回改写后的字节流。
//
// 作用：本代理"页面和原站一样可用"的核心——把所有指向外部的 URL
// 收敛到代理入口，并注入运行时 JS 桩。
// 场景：doProxy 处理上游 HTML 响应时调用。
// 入参：
//   - raw：上游返回的原始 HTML 字节流（编码未知）。
//   - base：文档自身的 URL，所有相对地址以此为解析基准。
//   - myHost：代理自身的 host（localhost:8765 之类）；指向它的
//     地址视为"已在代理内"，不再改写。
//   - js：由 buildProxyJS 生成的代理桩源码，注入 <head> 末尾。
//   - subdomainMode：是否子域名模式（同源地址改写策略不同）。
//
// 出参：
//   - []byte：改写后的 HTML。
//   - error：解析/渲染失败（调用方此时应回退为原样返回）。
func rewriteHTML(raw []byte, base *url.URL, myHost string, js string, subdomainMode bool) ([]byte, error) {
	// charset.NewReader 按 Content-Type / <meta> / BOM 探测编码并
	// 包成 UTF-8 reader；探测失败则回退按原文处理。
	cr, err := charset.NewReader(bytes.NewReader(raw), "")
	if err != nil {
		cr = bytes.NewReader(raw)
	}
	// 容错解析：html.Parse 从不返回部分树，异常时 doc 仍可用
	doc, err := html.Parse(cr)
	if err != nil {
		return nil, err
	}
	// walk 是递归 DOM 遍历闭包：每访问一个元素节点做三类处理
	// （剥统计脚本 / 重写内联样式 / 重写 URL 属性），再递归子节点。
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		// 分支：只处理元素节点；文本/注释/文档节点不改写
		if n.Type == html.ElementNode {
			// 处理一：剥离第三方访客统计脚本（如 busuanzi 不蒜子）。
			// 这类脚本会读取自身加载地址拼出 /api.php 发起 POST 再做
			// .json() 解析，而目标站通常没有该端点、返回 HTML，导致
			// 控制台 "Unexpected token '<'" 报错。它与代理功能无关，
			// 剥离后对目标站主功能零影响。
			if n.Data == "script" {
				for _, a := range n.Attr {
					// 分支：只关心 src 属性，内联脚本无 src 可剥
					if a.Key != "src" {
						continue
					}
					av := strings.TrimSpace(a.Val)
					// 分支：空 src 或应跳过的协议（data: 等）——跳过
					if av == "" || shouldSkip(av) {
						continue
					}
					// 以文档为基准解析出绝对地址再判定域名特征
					abs := resolveURL(base, av)
					low := strings.ToLower(abs)
					// 分支：命中 busuanzi 特征串——从父节点上摘除自身
					// 并终止对本节点的后续处理（节点已不在树中，
					// 继续改属性没有意义）
					if strings.Contains(low, "busuanzi") {
						if n.Parent != nil {
							n.Parent.RemoveChild(n)
						}
						return
					}
				}
			}
			// 处理二：<style> 块——对每个文本子节点做 CSS url() 改写
			if n.Data == "style" {
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					// 分支：只改文本子节点（<style> 内不应有其他元素）
					if c.Type == html.TextNode {
						c.Data = rewriteCSS(c.Data, base, subdomainMode)
					}
				}
			}
			// 处理三：逐属性改写。先看 style（CSS 整体改写），
			// 再按 urlAttrs 表看 URL 类属性。
			for i := range n.Attr {
				// 取地址：Attr 是值切片，用下标取指针以便原地赋值
				a := &n.Attr[i]
				// 分支：style=""——CSS 片段整体改写后 continue
				if a.Key == "style" {
					a.Val = rewriteCSS(a.Val, base, subdomainMode)
					continue
				}
				// 分支：非 URL 属性（class/id/alt...）——不碰
				if !urlAttrs[a.Key] {
					continue
				}
				v := strings.TrimSpace(a.Val)
				// 分支：空值或应跳过（#/javascript:/已是代理路径）——不碰
				if v == "" || shouldSkip(v) {
					continue
				}
				// 解析为绝对地址并二次校验协议
				abs := resolveURL(base, v)
				// 分支：无法解析——保持原值
				if abs == "" {
					continue
				}
				u2, e := url.Parse(abs)
				// 分支：解析异常——保持原值
				if e != nil {
					continue
				}
				// 分支：非 http/https 协议——保持原值
				if u2.Scheme != "http" && u2.Scheme != "https" {
					continue
				}
				// 分支：指向代理自身（myHost，如子域模式下根相对资源
				// 解析出的本地地址）——已在代理内，保持不变
				if u2.Host == myHost {
					continue
				}
				// 分支：子域名模式且同源——去掉 origin 改根相对路径，
				// 让浏览器在子域名源下解析、由子域名处理器映射回目标。
				// 这样 import('/x')、fetch('/api')、<script src="/x">
				// 都能正确落在子域名上。
				if subdomainMode && u2.Host == base.Host {
					rel := u2.Path
					// 分支：无路径时用 "/"
					if rel == "" {
						rel = "/"
					}
					// 分支：带查询串——拼回去
					if u2.RawQuery != "" {
						rel += "?" + u2.RawQuery
					}
					a.Val = rel
					continue
				}
				// 默认：异源地址改写为 basePath 前缀的 base64 路径
				a.Val = proxyURL(abs)
			}
		}
		// 递归处理全部子节点（元素与混合内容节点）
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	// 从文档根开始遍历整棵树
	walk(doc)
	// 遍历完成后统一注入 JS 代理桩（不参与遍历，避免被改写逻辑碰到）
	injectProxyScript(doc, js)
	// 重新渲染为字节流。html.Render 可能会规范化部分属性写法
	// （补引号、闭合标签），对功能无影响。
	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// injectProxyScript 以 DOM 方式把 JS 代理桩插入 <head> 末尾。
//
// 作用：让页面运行期发起的网络请求（fetch/XHR/WebSocket/window.open/
// form 提交）也被改写走代理，覆盖静态改写够不到的运行时行为。
// 场景：rewriteHTML 完成 DOM 改写后调用。
//
// 实现约束（必须走 DOM）：字节级搜索 </head> 可能命中内联
// <script>/<style> 里的字面量（如模板字符串 '</head>'、注释里的
// </head>），会把桩插进页面自身脚本内部——被注释吞掉=静默失效，
// 落进字符串=引号失衡 SyntaxError。DOM 节点追加在结构上不可能
// 落进脚本文本区，从根上杜绝该问题。
// 入参：
//   - doc：已解析的文档树（原地修改）。
//   - js：代理桩源码；空串时直接跳过（不注入空脚本节点）。
//
// 出参：无。
func injectProxyScript(doc *html.Node, js string) {
	// 分支：无桩可注——保持文档原样
	if js == "" {
		return
	}
	// 优先挂 <head>；没有则退回 <html>；再没有则挂文档根本身，
	// 三级兜底保证任何形态的 HTML 都能注入。
	head := findFirstElement(doc, "head")
	// 分支：无 <head>（畸形 HTML）——找 <html>
	if head == nil {
		head = findFirstElement(doc, "html")
	}
	// 分支：连 <html> 都没有——直接挂文档根
	if head == nil {
		head = doc
	}
	// 构造 <script> 元素并塞入文本子节点（TextNode 不需要转义，
	// Go 的 html.Render 对脚本元素不做实体编码，源码原样落到页面上）
	n := &html.Node{Type: html.ElementNode, Data: "script"}
	n.AppendChild(&html.Node{Type: html.TextNode, Data: js})
	// 追加到 <head> 末尾：此时页面自有脚本多已声明，桩的后置
	// 注入保证其包装的 fetch/XHR 覆盖页面后续初始化。
	head.AppendChild(n)
}

// findFirstElement 在 DOM 树中深度优先查找第一个指定标签的元素节点。
//
// 作用：为 injectProxyScript 定位注入挂载点。
// 场景：仅由 injectProxyScript 调用。
// 入参：
//   - n：遍历起点（通常为文档根）。
//   - tag：目标标签名（小写），如 "head"、"html"。
//
// 出参：
//   - *html.Node：首个匹配的元素节点；找不到返回 nil。
func findFirstElement(n *html.Node, tag string) *html.Node {
	// 分支：当前节点即目标——命中返回
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	// 递归子节点：深度优先，先到先得（文档序）
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if r := findFirstElement(c, tag); r != nil {
			return r
		}
	}
	// 整棵子树都没有——返回 nil
	return nil
}

// buildProxyJS 生成注入页面用的 JS 代理桩源码。
//
// 作用：产出"包住"浏览器网络 API 的一小段 IIFE 脚本：拦截 fetch /
// XMLHttpRequest / WebSocket / window.open / form submit，把目标
// URL 改写成代理地址后再放行，覆盖静态改写够不到的运行时请求。
// 场景：doProxy 转发 HTML 前调用，产出随 rewriteHTML 注入。
// 入参：
//   - base：目标站点 origin（如 "https://www.example.com"）；
//     子域名模式下 JS 端会以 location.origin 动态覆盖它。
//   - subdomainMode：true 时 BASE 取 location.origin、同源请求直接
//     改根相对路径（与 rewriteHTML 的子域策略保持一致）。
//
// 出参：
//   - string：完整 JS 源码；参数经 json.Marshal 转义，杜绝注入
//     与引号失衡。
//
// 桩内关键概念（与前端约定一一对应）：
//   - PREFIX：basePath，如 "/p/"；
//   - BASE：解析相对 URL 的基准 origin；
//   - BASE_HOST：同源判定用的 host；
//   - SUBMODE：是否子域名模式；
//   - enc(u)：TextEncoder -> btoa -> base64url（手动把 +/ 换成 -_、
//     去尾部 '='），与 Go 端 encodeURL 的 RawURLEncoding 完全一致；
//   - px(u)：改写入口——相对地址先 abs()，非 http(s) 原样返回，
//     子域模式且同源时返回 pathname+search+hash，否则 PREFIX+enc()。
//
// 说明：ws:// 按设计不透传（服务端未实现 WS 通道），桩对 ws/wss
// 地址照样 px() 化，连接失败属预期。
func buildProxyJS(base string, subdomainMode bool) string {
	// 四个配置量各自 JSON 序列化后替换进模板：用 JSON 而不是裸
	// 字符串拼接，可保证引号/斜杠被正确转义，源头防 JS 注入。
	prefixJSON, _ := json.Marshal(basePath) // "/p/"
	baseJSON, _ := json.Marshal(base)       // 目标 origin
	baseHost := ""                          // 目标 host（子域同源判定用）
	if u, e := url.Parse(base); e == nil {
		baseHost = u.Host
	}
	baseHostJSON, _ := json.Marshal(baseHost)
	submodeJSON, _ := json.Marshal(subdomainMode)
	// 桩源码模板：占位符 {{PREFIX}}/{{BASE}}/{{BASEHOST}}/{{SUBMODE}}
	// 由下方四行替换为 JSON 字面量（含两端引号）。
	// 结构总览：
	//   1) 定义配置常量与 enc/abs/px 三个工具函数；
	//   2) 包住 window.fetch：字符串参数先 px()，Request 对象重建；
	//   3) 包住 XMLHttpRequest.prototype.open：URL 参数先 px()；
	//     第三参数 async 未显式传时补 true（保持原生默认行为）；
	//   4) 包住 window.WebSocket：URL 先 px()（ws:// 不透传属预期）；
	//   5) 包住 window.open：URL 先 px()；
	//   6) capture 阶段监听 submit：form action 先 px()；若 action
	//      本来就指向当前 origin（own origin）则不动，避免误伤
	//      站点自身的本地表单端点。
	tmpl := `(function(){var PREFIX={{PREFIX}};var BASE={{BASE}};var BASE_HOST={{BASEHOST}};var SUBMODE={{SUBMODE}};if(SUBMODE){BASE=location.origin;BASE_HOST=location.host;}function enc(u){var b=new TextEncoder().encode(u),s='';for(var i=0;i<b.length;i++)s+=String.fromCharCode(b[i]);return btoa(s).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');}function abs(u){try{return new URL(u,BASE).href;}catch(e){return u;}}function px(u){var a=abs(u);if(!/^https?:/i.test(a))return u;if(SUBMODE){try{var t=new URL(a);if(t.host===BASE_HOST){return t.pathname+t.search+t.hash;}}catch(e){}}return PREFIX+enc(a);}var _f=window.fetch;window.fetch=function(i,o){if(typeof i==='string')return _f(px(i),o);if(i instanceof Request){try{i=new Request(px(i.url),i);}catch(e){}}return _f(i,o);};var _o=XMLHttpRequest.prototype.open;XMLHttpRequest.prototype.open=function(m,u,a){return _o.call(this,m,px(u),a===undefined?true:a);};var _w=window.WebSocket;window.WebSocket=function(u,p){if(typeof u==='string'){try{u=px(u);}catch(e){}}return p!==undefined?new _w(u,p):new _w(u);};var _ow=window.open;window.open=function(u,n,s){return _ow(px(u),n,s);};document.addEventListener('submit',function(e){var f=e.target;if(f&&f.tagName==='FORM'&&f.action){try{var a=new URL(f.action,BASE);if(a.host===location.host)return;f.action=px(f.action);}catch(err){}}},true);})();`
	// 逐个替换占位符为对应 JSON 字面量（各出现一次，Replace 计数 1）
	t := strings.Replace(tmpl, "{{PREFIX}}", string(prefixJSON), 1)
	t = strings.Replace(t, "{{BASE}}", string(baseJSON), 1)
	t = strings.Replace(t, "{{BASEHOST}}", string(baseHostJSON), 1)
	t = strings.Replace(t, "{{SUBMODE}}", string(submodeJSON), 1)
	return t
}
