// 本文件是 webproxy 的转发核心，包含四大部分：
//
//  1. 入口路由（rootHandler 及三个分支处理器）：把请求按形态分流到
//     子域名反代 / base64 路径代理 / UI 页；
//  2. 反代内核（doProxy）：SSRF 校验 -> 上游请求（带服务端会话 jar）
//     -> 3xx 重定向改写 -> HTML/CSS 内容改写；
//  3. WebSocket 隧道（proxyWebSocket）：基于 httputil.ReverseProxy
//     透传升级连接，并在服务端注入会话 Cookie；
//  4. 调试日志（dbg*）：环境变量开关，排障用。
//
// 设计要点回顾：
//   - 会话 Cookie 由服务端按 origin 持有（jarFor），不下发浏览器，
//     绕开跨站 iframe 的 SameSite 扣发问题（详见 jarFor 注释）；
//   - Origin/Referer 改写为上游源，兼容按源站做校验的目标（fnOS 等）。
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// isHopHeader 判断是否为 HTTP/1.1 逐跳头（hop-by-hop header）。
//
// 作用：逐跳头只对单段连接有意义，代理转发时必须剔除。
// 场景：doProxy 构造上游请求头时逐项过滤。
// 入参：
//   - k：原始头名称（大小写任意）。
//
// 出参：
//   - bool：true 表示该头禁止向上游透传。
func isHopHeader(k string) bool {
	// 标准库同样提供 httpguts.HeaderContain 等判断，这里显式列举以保证
	// 行为透明可控。各头的含义：
	//   connection/proxy-connection/keep-alive：连接管理；
	//   proxy-authenticate/proxy-authorization：代理鉴权；
	//   te/trailer/transfer-encoding：分块传输与预告头；
	//   upgrade：协议升级（WebSocket 场景走单独隧道，不在此复制）。
	switch strings.ToLower(k) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// isRespSkip 判断上游响应头是否不得回传给浏览器。
//
// 作用：响应侧的第二道过滤——除逐跳头外，还有一类头会破坏代理
// 的页面嵌入与重定向改写语义，也必须拦下。
// 场景：doProxy 复制响应头给浏览器之前逐项过滤。
// 入参：
//   - k：原始头名称。
//
// 出参：
//   - bool：true 表示不回传。
//
// 除逐跳头外，以下头的拦截理由：
//   - x-frame-options / frame-ancestors 类 CSP / COOP：目标站禁止被
//     iframe 嵌入的策略头；不回传才能让页面在 UI 外框 iframe 中显示；
//   - content-length：改写后的 HTML/CSS 长度与上游不同，回传旧值会
//     造成浏览器截断或挂起，必须让 Go 按实际输出重新计算；
//   - location：3xx 目标由 doProxy 自己改写后单独下发（见 doProxy
//     的 30x 分支），不透传上游原值。
func isRespSkip(k string) bool {
	if isHopHeader(k) {
		return true
	}
	switch strings.ToLower(k) {
	case "x-frame-options", "content-security-policy",
		"content-security-policy-report-only", "cross-origin-opener-policy",
		"content-length", "location":
		return true
	}
	return false
}

// makeTransport 基于 SSRF 校验阶段解析出的 IP 列表构造 HTTP Transport。
//
// 作用：把"域名 -> IP"的解析结果固化到 DialContext，让请求只连已
// 校验过的地址。
// 场景：doProxy / proxyWebSocket 每次请求都会新建（DisableKeepAlives
// 使 Transport 无连接复用，新建成本可忽略）。
// 入参：
//   - ips：ssrfCfg.resolveAndCheck 返回并检查通过的 IP 列表，按顺序
//     尝试拨号（等价于按 DNS 应答优先级）。
//
// 出参：
//   - *http.Transport：拨号绑定上述 IP 的 Transport。
//
// 关键行为：
//   - DialContext 忽略 addr 里的域名、只用 ips 拨号：防止 TOCTOU
//     （双重解析）——SSRF 检查与真实连接之间域名若被换成内网地址，
//     绑定 IP 后攻击面即被关闭；
//   - TLS：最低 TLS 1.2；证书校验默认开启，InsecureSkipVerify 由
//     配置决定（访问自签证书的内网站点时开启）；
//   - ForceAttemptHTTP2=false：WebSocket 升级要求 HTTP/1.1，
//     禁用 HTTP/2 尝试，避免升级协商失败；
//   - DisableKeepAlives=true：代理目标千差万别，不复用连接可避免
//     空闲连接堆积与跨目标连接误用。
func makeTransport(ips []net.IP) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// 从标准库传入的 host:port 中只取端口，主机部分一律用
			// 上面校验过的 ips（addr 里的域名故意忽略）
			_, port, err := net.SplitHostPort(addr)
			// 分支：addr 异常时回落 80（https 场景由 TLS 拨号自行处理）
			if err != nil {
				port = "80"
			}
			// lastErr 记住最后一个失败原因，全部 IP 试完再返回
			var lastErr error
			// 按 DNS 应答顺序逐个尝试，任一成功即用（与标准库行为一致）
			for _, ip := range ips {
				c, e := net.DialTimeout(network, net.JoinHostPort(ip.String(), port), 10*time.Second)
				// 分支：拨号成功——立即返回该连接
				if e == nil {
					return c, nil
				}
				lastErr = e
			}
			// 分支：全部 IP 均失败——报最后一个错误
			return nil, lastErr
		},
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false, // WebSocket 升级需要 HTTP/1.1，禁用 HTTP/2 尝试
	}
}

// isWebSocket 判断请求是否为 WebSocket 升级请求
func isWebSocket(r *http.Request) bool {
	// Upgrade 头必须含 websocket（大小写不敏感）
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	// Connection 头需含 upgrade（有的客户端写成 "keep-alive, Upgrade"，
	// 用包含判断而非相等判断）
	return strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// rootHandler 统一入口：
//   - 子域名 <base64url(origin)>.localhost:<port>  → 子域名反代（解决 SPA 绝对路径 import/fetch）
//   - 路径以 base_path(/p/) 开头                    → 传统整 URL base64 反代（兼容）
//   - 其它                                          → UI 控制页
func rootHandler(w http.ResponseWriter, r *http.Request) {
	// 分支一：路径以 basePath（/p/）开头——base64 整 URL 反代
	if strings.HasPrefix(r.URL.Path, basePath) {
		proxyHandler(w, r)
		return
	}
	// 分支二：Host 是 <hex>.localhost 子域名形式——子域名反代
	if isSubdomain(r.Host) {
		proxySubdomain(w, r)
		return
	}
	// 分支三：其余请求——UI 控制页（含 favicon 204 短路）
	uiHandler(w, r)
}

// isSubdomain 判断 Host 是否为本代理的子域名反代形式：
// <hex(origin)>.localhost[:port]
// 注意：浏览器会把主机名规范化为小写，因此子域名标签必须用大小写无关编码。
// 这里用十六进制（0-9a-f 小写化无损），而 /p/ 的 base64url 仅用于 path/query，
// 不被小写化，故仍可保留。
func isSubdomain(host string) bool {
	h := host
	// 去掉端口部分，只留主机名（如 "abcd.localhost:8765" -> "abcd.localhost"）
	if i := strings.Index(h, ":"); i >= 0 {
		h = h[:i]
	}
	// 分支：不是 .localhost 结尾——不可能是本代理子域
	if !strings.HasSuffix(h, ".localhost") {
		return false
	}
	// 取子域标签（.localhost 前面的部分）
	label := strings.TrimSuffix(h, ".localhost")
	// 分支：空标签（即请求直接打 localhost）——是控制页入口，不是子域
	if label == "" {
		return false
	}
	// 分支：标签必须是合法十六进制串——否则可能是用户自建的
	// *.localhost 站点，不属于本代理，交回 UI 处理。
	// 这也是"任意 <hex>.localhost 都视为代理地址"的判定核心。
	_, err := hex.DecodeString(label)
	return err == nil
}

// decodeSubdomainOrigin 从 Host 解出目标 origin（scheme://host:port）
func decodeSubdomainOrigin(host string) *url.URL {
	h := host
	// 去端口（同 isSubdomain 的处理）
	if i := strings.Index(h, ":"); i >= 0 {
		h = h[:i]
	}
	// 分支：非 .localhost 后缀——无从解起
	if !strings.HasSuffix(h, ".localhost") {
		return nil
	}
	// 取出十六进制标签
	label := strings.TrimSuffix(h, ".localhost")
	// 分支：hex 解码失败（非本代理生成的标签）——放弃
	raw, err := hex.DecodeString(label)
	if err != nil {
		return nil
	}
	// 解码结果应为完整 origin 字符串（如 "http://192.168.0.107:5666"）
	u, err := url.Parse(string(raw))
	// 分支：解析失败或要素不全（无 scheme/host）——视为非法子域
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	return u
}

// proxySubdomain 子域名反代：
// 文档源即目标，浏览器对 /x、./x、fetch('/api')、import('/x') 的解析都会落到
// 子域名下，被本处理器正确映射回目标，从而让重 SPA（如飞牛 fnOS）正常加载。
func proxySubdomain(w http.ResponseWriter, r *http.Request) {
	// 从 Host 反解目标 origin；失败说明有人伪造/误用了子域格式
	origin := decodeSubdomainOrigin(r.Host)
	// 分支：解码失败——直接 400，不猜目标
	if origin == nil {
		http.Error(w, "非法子域名代理请求", 400)
		return
	}
	// 用 origin 的 scheme/host 拼上本次请求的 path/query，得到完整目标
	target := *origin
	target.Path = r.URL.Path
	// 分支：路径为空时补 "/"（url.URL 序列化时空 Path 不规范）
	if target.Path == "" {
		target.Path = "/"
	}
	// 查询串逐个拷入并剔除内部标记
	q := r.URL.Query()
	q.Del("_wpframe") // 内部标记，不向上游透传
	target.RawQuery = q.Encode()

	// 顶层导航改道到控制页（带 UI 外框），与 /p/ 行为一致。
	// 注意：必须显式指向控制页主机(localhost:<port>)，不能用相对路径——
	// 否则相对 302 会落在子域名主机上，导致新标签再次丢失外框。
	// 判定"顶层导航"：GET 且未带 _wpframe=1 内部标记。
	if r.Method == http.MethodGet && r.URL.Query().Get("_wpframe") != "1" {
		// 用 Sec-Fetch-Dest 识别导航请求；老浏览器没有该头时退回
		// Accept 含 text/html 的启发式判断。
		dest := r.Header.Get("Sec-Fetch-Dest")
		topNav := dest == "document" ||
			(dest == "" && strings.HasPrefix(r.Header.Get("Accept"), "text/html"))
		// 分支：是顶层导航——302 到控制页 ?url=<base64(target)>
		if topNav {
			// 保留原始端口，避免 8765 之外的部署场景改丢端口
			portPart := ""
			if i := strings.Index(r.Host, ":"); i >= 0 {
				portPart = r.Host[i:]
			}
			http.Redirect(w, r, "http://localhost"+portPart+"/?url="+encodeURL(target.String()), http.StatusFound)
			return
		}
	}
	// 其余请求（iframe 内文档、XHR、静态资源）直接走反代内核；
	// 子域模式下 JS 桩以 location.origin 为 BASE（buildProxyJS 第二参数 true）。
	doProxy(w, r, &target, buildProxyJS(target.String(), true))
}

// proxyHandler 传统整 URL base64 反代（/p/<base64url>）
func proxyHandler(w http.ResponseWriter, r *http.Request) {
	// 剥掉前缀并去除多余斜杠，得到 base64 段
	enc := strings.TrimPrefix(r.URL.Path, basePath)
	enc = strings.Trim(enc, "/")
	// 分支：空段——连目标地址都没有，400
	if enc == "" {
		http.Error(w, "缺少目标地址", 400)
		return
	}

	// 顶层导航改道：/p/xxx 被新标签页/地址栏直接打开时（没有 UI 外框），
	// 302 到 /?url=xxx 由外框页通过 iframe 加载，保证任何新标签都有地址栏外框。
	// iframe 内的加载（Sec-Fetch-Dest: iframe/frame，或 UI 自带 ?_wpframe=1 标记）不受影响。
	// 判定条件与 proxySubdomain 一致：GET 且非内部标记的顶层导航。
	if r.Method == http.MethodGet && r.URL.Query().Get("_wpframe") != "1" {
		dest := r.Header.Get("Sec-Fetch-Dest")
		topNav := dest == "document" ||
			(dest == "" && strings.HasPrefix(r.Header.Get("Accept"), "text/html"))
		// 分支：顶层导航——带 enc 原样跳控制页（base64 段本身就是 URL 内容）
		if topNav {
			http.Redirect(w, r, "/?url="+enc, http.StatusFound)
			return
		}
	}

	// base64url 解码（RawURLEncoding：无 '=' 填充，与前端 enc() 对应）
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	// 分支：解码失败——格式非法
	if err != nil {
		http.Error(w, "目标地址解码失败", 400)
		return
	}
	target, err := url.Parse(string(raw))
	// 分支：URL 非法或缺 scheme/host——不是可代理的目标
	if err != nil || target.Scheme == "" || target.Host == "" {
		http.Error(w, "目标地址无效", 400)
		return
	}
	// 分支：目标就是代理自身——防止自指死循环
	if target.Host == r.Host {
		http.Error(w, "禁止请求代理自身", 400)
		return
	}

	// 进入反代内核；路径模式（非子域）下 JS 桩第二参数为 false
	doProxy(w, r, target, buildProxyJS(target.String(), false))
}

// doProxy 公共反代核心：SSRF 校验 → 上游请求 → 头透传 → HTML/CSS 重写或透传
func doProxy(w http.ResponseWriter, r *http.Request, target *url.URL, stubJS string) {
	// 归一化 ws/wss → http/https：WebSocket 隧道建立在 HTTP 之上，
	// 这样 SSRF 校验与上游拨号都能按普通 HTTP 处理（ws 协议本身不在白名单内）。
	// wasWS 必须先记录：scheme 改写后就拿不到原始信息了。
	wasWS := isWebSocket(r)
	switch target.Scheme {
	// 分支：明文 WS —— 归一化为 http
	case "ws":
		target.Scheme = "http"
	// 分支：TLS WS —— 归一化为 https
	case "wss":
		target.Scheme = "https"
	}

	// SSRF 闸门：协议与 IP 分类检查，失败直接 403 并说明原因
	ips, err := ssrfCfg.resolveAndCheck(target)
	// 分支：被拦截——403 + 中文原因（不发向上游）
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(403)
		fmt.Fprintf(w, "SSRF 拦截: %v\n", err)
		return
	}

	// 分支：WebSocket 升级请求——走独立隧道处理（不走下面的普通请求流）
	if wasWS {
		proxyWebSocket(w, r, target, ips)
		return
	}

	// 请求体：仅非 GET/HEAD 且确实有 body 时才透传（GET 带 body 无意义，
	// 且 Go 的 NewRequest 对 GET body 会拒绝）
	var body io.Reader
	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		body = r.Body
	}
	// 构造上游请求：方法、目标、体三者就位
	outReq, err := http.NewRequest(r.Method, target.String(), body)
	// 分支：构造失败——500（一般是 method 字符串非法）
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// 复制请求头：逐跳头剔除，Accept-Encoding 也剔除（下面强制 identity，
	// 因为改写 HTML/CSS 必须拿到明文，压缩流无法正确改写）。
	for k, vs := range r.Header {
		if isHopHeader(k) {
			continue
		}
		if strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		for _, v := range vs {
			outReq.Header.Add(k, v)
		}
	}
	// 强制不压缩：改写器需要可读的 HTML/CSS
	outReq.Header.Set("Accept-Encoding", "identity")

	// 反向代理场景下，把浏览器发出的 Origin/Referer 改写为上游自身源，
	// 避免 fnOS 等后端基于源站做签名/防 CSRF 校验时因看到代理子域名而拒绝（验签失败）。
	// 注意：浏览器侧的同源判断不受影响——它看到的是子域名文档源，仍按同源处理，无 CORS 预检。
	upstreamOrigin := target.Scheme + "://" + target.Host
	// 分支：带 Origin（POST/PUT/CORS 场景常见）——替换为上游源
	if orig := outReq.Header.Get("Origin"); orig != "" {
		outReq.Header.Set("Origin", upstreamOrigin)
	}
	// 分支：带 Referer——保留路径，仅替换源站部分（见 rewriteOriginInURL）
	if ref := outReq.Header.Get("Referer"); ref != "" {
		outReq.Header.Set("Referer", rewriteOriginInURL(ref, target))
	}

	// 调试日志：改动后的 Origin/Referer，以及原始请求摘要（Cookie 截断防泄露刷屏）
	dbg("[OUT.h] Origin=%q Referer=%q", outReq.Header.Get("Origin"), outReq.Header.Get("Referer"))
	dbg("[REQ] %s %s -> %s%s", r.Method, r.Host, target.Host, r.URL.Path)
	dbg("[REQ.h] Origin=%q Referer=%q Auth=%q XRW=%q CT=%q Cookie=%q",
		r.Header.Get("Origin"), r.Header.Get("Referer"),
		truncate(r.Header.Get("Authorization"), 80),
		r.Header.Get("X-Requested-With"), r.Header.Get("Content-Type"),
		truncate(r.Header.Get("Cookie"), 100))

	// 会话 Cookie 由代理服务端 jar 持有（见 jarFor），不再依赖浏览器存储：
	// 页面以 iframe 内嵌（localhost 顶层 + <hex>.localhost 子域）在 Chrome 看来是
	// 跨站上下文，SameSite=Lax 的会话 Cookie 会被浏览器扣发（不发往 iframe 内的
	// fetch/XHR/WS），导致 fnOS 登录成功后 /app/token 仍 401、验签失败连环 reload。
	// 服务端 jar 完全绕开浏览器第三方 Cookie 策略，且 WebSocket 隧道也能注入同一份 Cookie。
	client := &http.Client{
		// 拨号绑定 SSRF 校验过的 IP 列表
		Transport: makeTransport(ips),
		// 按目标 origin 取/建会话 jar：登录态由服务端自动维护
		Jar: jarFor(target.Scheme + "://" + target.Host),
		// 重定向不自动跟随：3xx 交给下方改写分支处理，转发给浏览器
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		// 整体超时 120s：覆盖慢内网站点；WS 不走此 client 不受影响
		Timeout: 120 * time.Second,
	}

	// 发出上游请求
	resp, err := client.Do(outReq)
	// 分支：上游不可达/超时——502 + 原因
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(502)
		fmt.Fprintf(w, "上游请求失败: %v\n", err)
		return
	}
	defer resp.Body.Close()

	// 3xx 分支：手动改写 Location 头为代理地址后转发 302。
	// 因 isRespSkip 拦掉了上游 Location，这里必须自己补。
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		// 分支：有 Location 且能解析为绝对地址——改写并直接返回
		if loc != "" {
			if abs := resolveURL(target, loc); abs != "" {
				w.Header().Set("Location", proxyURL(abs))
				w.WriteHeader(302)
				return
			}
		}
		// 分支：没有/不可解析 Location——按无 Location 的 3xx 正常下发
	}

	// 调试日志：响应摘要；Set-Cookie 一并打印（截断）便于核对服务端 jar 收集情况
	dbg("[RESP] %s %s%s -> %d ct=%q sc=%q",
		r.Method, target.Host, r.URL.Path, resp.StatusCode,
		resp.Header.Get("Content-Type"), truncate(strings.Join(resp.Header.Values("Set-Cookie"), " | "), 200))
	// ≥400 的响应把 body 读出来打日志（限长 800）后放回流：
	// fnOS 验签失败（errno:65534）是 HTTP 200 + JSON 体，真正的 4xx/5xx
	// 走这条能直接在日志里看到上游原文。HTML/CSS 的 4xx 不改写不记录，
	// 避免把大段错误页刷进日志。
	if resp.StatusCode >= 400 && !strings.Contains(resp.Header.Get("Content-Type"), "text/html") && !strings.Contains(resp.Header.Get("Content-Type"), "text/css") {
		if b, e := io.ReadAll(resp.Body); e == nil {
			dbg("[RESP.body] %s", truncate(string(b), 800))
			resp.Body = io.NopCloser(bytes.NewReader(b))
		}
	}

	// 复制响应头给浏览器：过滤头剔除；Set-Cookie 一律不下发
	// （服务端 jar 已收集，详见下方注释）
	for k, vs := range resp.Header {
		if isRespSkip(k) {
			continue
		}
		// 分支：Set-Cookie —— 会话 Cookie 统一保存在服务端 jar
		// （client.Jar 自动收集，见 jarFor），不再下发浏览器：跨站
		// iframe 上下文里浏览器会扣发 SameSite Cookie，下发了也用不上，
		// 反而让登录态看起来"时有时无"。
		if strings.EqualFold(k, "Set-Cookie") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	// 取内容类型，决定改写策略
	ct := resp.Header.Get("Content-Type")

	// 分支一：HTML —— 全量读内存后交给 rewriteHTML 改写。
	// 已知取舍：大文档会进内存；视频/二进制不走此分支（Content-Type 不匹配）。
	if strings.Contains(ct, "text/html") {
		data, _ := io.ReadAll(resp.Body)
		out, err := rewriteHTML(data, target, r.Host, stubJS, isSubdomain(r.Host))
		// 分支：改写失败（解析/渲染异常）——回退为原始 HTML 原样返回，
		// 保证"最坏也能看到没改写的页面"而不是 500。
		if err != nil {
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(resp.StatusCode)
			w.Write(data)
			return
		}
		// 统一声明 UTF-8：rewriteHTML 输出必然是 UTF-8（charset 探测已转）
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		w.Write(out)
		return
	}
	// 分支二：CSS —— 同样全量读内存后改写 url()
	if strings.Contains(ct, "text/css") {
		data, _ := io.ReadAll(resp.Body)
		out := rewriteCSS(string(data), target, isSubdomain(r.Host))
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		w.Write([]byte(out))
		return
	}

	// 分支三：其余内容（JS/图片/字体/二进制）——流式拷贝，不落地内存
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ---- 服务端会话 Cookie jar ----
// 按目标 origin 全局保存上游会话 Cookie（单用户自用代理，多目标互不干扰）。
// 关键：Go 默认 cookiejar 对"无 Domain 属性的 host-only Cookie"（fnOS 正是这种：
// ost=...; Path=/; HttpOnly; SameSite=Lax）按上游主机正常存储，IP 主机也没问题；
// 之前探测失败是因为显式 Domain=IP 的 Cookie 本身非法（RFC 禁止 IP 作 Domain）。
// 注意：jar 为进程内内存结构，代理重启即丢失，届时需重新登录目标站。
var jars sync.Map

// jarFor 按 origin 获取（不存在则创建）服务端 Cookie jar。
//
// 作用：让 http.Client 自动收集/回填某目标站的会话 Cookie。
// 场景：doProxy 构造 client 时按目标 origin 取 jar；proxyWebSocket
// 经 cookiesForJar 读取同一 jar 注入 WS 握手。
// 入参：
//   - origin：目标源，形如 "http://192.168.0.107:5666"（scheme 必须带，
//     cookiejar 的 SetCookies 对是否 Secure 的判断依赖它）。
//
// 出参：
//   - http.CookieJar：该 origin 对应的 jar，永不为 nil。
func jarFor(origin string) http.CookieJar {
	// 快路径：已存在直接返回
	if v, ok := jars.Load(origin); ok {
		return v.(http.CookieJar)
	}
	// 慢路径：新建并登记（并发首次创建时后写覆盖先写，内容等价无影响）
	jar, _ := cookiejar.New(nil)
	jars.Store(origin, jar)
	return jar
}

// cookiesForJar 取出某 origin 已存的会话 Cookie（用于 WebSocket 握手注入）。
func cookiesForJar(origin string, u *url.URL) string {
	v, ok := jars.Load(origin)
	// 分支：该 origin 还没有 jar——无 Cookie 可注入
	if !ok {
		return ""
	}
	// cookiejar 内部按 URL 的 scheme/host/path 过滤过期与作用域不符的项，
	// 这里传目标 URL 让库代劳，避免自己判断 Secure/Path 规则。
	cks := v.(http.CookieJar).Cookies(u)
	// 分支：没有任何有效 Cookie
	if len(cks) == 0 {
		return ""
	}
	// 预分配切片容量，逐个拼 "name=value"
	parts := make([]string, 0, len(cks))
	for _, c := range cks {
		parts = append(parts, c.Name+"="+c.Value)
	}
	// 按 HTTP 头惯例用 "; " 连接
	return strings.Join(parts, "; ")
}

// proxyWebSocket 透传 WebSocket 升级：复用 httputil.ReverseProxy，
// 它会转发 Upgrade 请求、在收到后端 101 后劫持双向连接并拷贝字节，
// 实现真正的 WebSocket 隧道（而非当普通 HTTP 抓取）。
func proxyWebSocket(w http.ResponseWriter, r *http.Request, target *url.URL, ips []net.IP) {
	// 注入服务端 jar 中同一份会话 Cookie：浏览器不会在跨站 iframe 里为 WS 握手
	// 携带 SameSite Cookie，登录态必须由代理在服务端补上。
	// 没有 Cookie 时不动请求头，保持浏览器原样（避免覆盖无意义空值）。
	if ck := cookiesForJar(target.Scheme+"://"+target.Host, target); ck != "" {
		r.Header.Set("Cookie", ck)
		dbg("[WS] 注入 Cookie: %q", truncate(ck, 100))
	}
	// ReverseProxy 自带 101 劫持与双向拷贝；这里只定制 Transport、
	// 目标改写与错误处理。
	rp := &httputil.ReverseProxy{
		// 复用 SSRF 绑定 IP 的 Transport（其 ForceAttemptHTTP2=false，
		// 正是 HTTP/1.1 升级所需要的）
		Transport: makeTransport(ips),
		// Director：把请求整体改写到目标上
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme // 已归一化为 http/https
			req.URL.Host = target.Host
			req.URL.Path = target.Path
			req.URL.RawQuery = target.RawQuery
			req.Host = target.Host
			// 让后端看到同源 Origin，提升兼容性（部分 WS 服务端会校验 Origin）
			req.Header.Set("Origin", target.Scheme+"://"+target.Host)
		},
		// 握手/隧道失败时给浏览器明确原因（502），便于前端识别
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(502)
			fmt.Fprintf(w, "WebSocket 代理失败: %v\n", err)
		},
	}
	rp.ServeHTTP(w, r)
}

// ---- 调试日志（设置环境变量 WEBPROXY_DEBUG 即开启；可选 WEBPROXY_DEBUG_FILE 落盘） ----
// dbgFile 是可选日志文件句柄；dbgOnce 保证文件只打开一次。
var (
	dbgFile *os.File
	dbgOnce sync.Once
)

// dbgEnabled 判断调试日志开关（WEBPROXY_DEBUG 环境变量非空即开）。
func dbgEnabled() bool { return os.Getenv("WEBPROXY_DEBUG") != "" }

// dbg 输出一行调试日志（stderr；配置了 WEBPROXY_DEBUG_FILE 时同步落盘）。
//
// 作用：默认零开销的排障钩子——未设置环境变量时每条 dbg 调用仅一次
// 函数进入即返回。
// 场景：请求进出、头改写、SSRF 结果、WS 注入等关键路径打点。
// 入参：
//   - format / args：同 fmt.Sprintf 的格式串与参数清单。
//
// 出参：无。
func dbg(format string, args ...interface{}) {
	// 分支：未开启调试——立即返回（热路径上唯一开销）
	if !dbgEnabled() {
		return
	}
	// 首次调用时打开日志文件（若配置了路径）；失败则仅用 stderr
	dbgOnce.Do(func() {
		if p := os.Getenv("WEBPROXY_DEBUG_FILE"); p != "" {
			if f, e := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); e == nil {
				dbgFile = f
			}
		}
	})
	// 先格式化一次，两处复用
	s := fmt.Sprintf(format, args...)
	// stderr 保证容器日志（docker logs）可见
	fmt.Fprintln(os.Stderr, s)
	// 分支：文件打开成功——同步落盘一份（重启排障用）
	if dbgFile != nil {
		fmt.Fprintln(dbgFile, s)
	}
}

// truncate 把字符串截断到 n 字符并在尾部追加 "...[trunc]"。
//
// 作用：日志脱敏/防刷屏（Cookie/Authorization/错误页等敏感或超长值）。
// 场景：dbg 打印所有截断场景。
// 入参：
//   - s：原始字符串。
//   - n：保留的最大字符数（按字节，中文可能截半角——仅日志用途可接受）。
//
// 出参：
//   - string：截断后的字符串；原长不超过 n 时原样返回。
func truncate(s string, n int) string {
	// 分支：没超长——原样
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[trunc]"
}

// rewriteOriginInURL 把一个可能为绝对 URL 的头部值（如 Referer）的源站部分替换为上游源站，
// 路径与查询保持不变。无法解析时原样返回。
func rewriteOriginInURL(raw string, target *url.URL) string {
	u, err := url.Parse(raw)
	// 分支：不是可解析的绝对 URL（相对值/空值）——原样返回，
	// 相对 Referer 一般是同源引用，改写反而失真
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	// 只换 scheme+host，path/query/fragment 原样保留
	u.Scheme = target.Scheme
	u.Host = target.Host
	return u.String()
}
