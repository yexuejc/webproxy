// 本文件是 webproxy 的单元测试集（package main，与源码同包，可直接
// 访问未导出符号，如 encodeURL / classify / truncate）。
//
// 覆盖范围（按被测试的模块划分）：
//   - rewriter.go：encodeURL/proxyURL/resolveURL 的编解码与 URL 解析；
//   - proxy.go：isSubdomain/decodeSubdomainOrigin 的子域判定、
//     rewriteOriginInURL 的头改写、truncate 的日志截断；
//   - ssrf.go：inCIDR 的网段判断、classify 的黑/白名单分类。
//
// 运行方式：go test ./... （cookietest 的 E2E 脚本走 build tag，
// 不在此列，见 tests/README.md）。
package main

import (
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/url"
	"strings"
	"testing"
)

// init 在测试进程启动时给包级变量兜底初值。
//
// 作用：go test 不会调用 main()，而 basePath 等变量是在 main() 里
// 赋值的；这里为空则补默认值，保证测试不依赖启动顺序。
// 场景：每个测试进程启动时执行一次。
func init() {
	// basePath 为空时补默认值 "/p/"（与 main.go 的缺省行为一致）
	if basePath == "" {
		basePath = "/p/"
	}
}

// TestEncodeURLRoundtrip 校验 base64url 编解码的往返一致性。
//
// 作用：encodeURL 是全代理地址拼接（/p/<enc>）与前端 JS enc() 的
// 公共契约，必须保证往返无损且无 '=' 填充。
// 场景：断言四类典型 URL（官网、带端口路径的内网站点、带查询串的
// 字体资源）都能编出去、解回来。
// 断言点：
//  1. 编码结果不含 '='（RawURLEncoding 无填充约定，前端也要去 '='）；
//  2. RawURLEncoding.DecodeString 能解回原串；
//  3. 解回结果与输入逐字节相等。
func TestEncodeURLRoundtrip(t *testing.T) {
	// cases 覆盖：普通官网、深层路径、内网+端口+查询串、长静态资源 URL
	cases := []string{
		"https://example.com/",
		"https://www.runoob.com/python/python-nested-loops.html",
		"http://192.168.0.107:5666/app/token?a=b&c=d",
		"https://static.jyshare.com/assets/font-awesome-4.7.0/fonts/fontawesome-webfont.woff2?v=4.7.0",
	}
	for _, u := range cases {
		// 编码
		enc := encodeURL(u)
		// /p/ 路径用的就是 RawURLEncoding，且不能有 '=' 填充（与前端 JS enc() 对齐）
		if strings.Contains(enc, "=") {
			t.Fatalf("encodeURL 含 '=' 填充，会破坏 base64url: %q", enc)
		}
		// 解码并比对
		raw, err := base64.RawURLEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("解码失败 %q: %v", enc, err)
		}
		if string(raw) != u {
			t.Fatalf("roundtrip 不一致: 入=%q 出=%q", u, string(raw))
		}
	}
}

// TestProxyURL 校验 proxyURL 拼出的代理路径形如 "<basePath><base64>"。
//
// 作用：proxyURL 是 rewriter.go 所有 URL 改写的最终产出形态，
// 拼接规则错则全站改写全错。
// 场景：固定 basePath 后比对拼接结果与手算期望值。
// 断言点：输出等于 "/p/" + encodeURL(目标)。
func TestProxyURL(t *testing.T) {
	// 先把全局变量钉死为默认前缀，避免其它测试改动后互相干扰
	basePath = "/p/"
	got := proxyURL("https://example.com/x")
	want := "/p/" + encodeURL("https://example.com/x")
	if got != want {
		t.Fatalf("proxyURL 错误: 得=%q 期望=%q", got, want)
	}
}

// TestResolveURL 校验相对/绝对/跨站三类引用的解析。
//
// 作用：resolveURL 是改写前把页面里的相对地址变绝对的第一步。
// 场景：以 "https://e.com/a/b" 为基准分别校验 "../c"（相对上级）、
// "/abs"（根相对）、跨站绝对地址。
// 断言点：三类输入分别解析为期望的绝对 URL。
func TestResolveURL(t *testing.T) {
	base, _ := url.Parse("https://e.com/a/b")
	// 相对上级路径："/a/b" 的上级是 "/c"
	if got := resolveURL(base, "../c"); got != "https://e.com/c" {
		t.Fatalf("相对解析错误: %q", got)
	}
	// 根相对路径：直接挂到 origin 下
	if got := resolveURL(base, "/abs"); got != "https://e.com/abs" {
		t.Fatalf("绝对解析错误: %q", got)
	}
	// 跨站绝对地址：原样保留（只解析不改写）
	if got := resolveURL(base, "https://other.com/z"); got != "https://other.com/z" {
		t.Fatalf("跨站解析错误: %q", got)
	}
}

// TestIsSubdomain 校验子域名反代入口的 Host 判定。
//
// 作用：isSubdomain 决定 rootHandler 把请求分去子域反代还是 UI，
// 误判会让整个反代链路走错分支。
// 场景：构造合法 hex 标签 + 一批非法形态。
// 断言点：
//   - "<hex>.localhost:8765" 判定为子域；
//   - 公网域名、裸 localhost、非 hex 标签（用户自建 *.localhost）、
//     带端口裸 localhost 均判定为非子域。
func TestIsSubdomain(t *testing.T) {
	// 用 encodeURL 同款 hex 编码生成合法标签（模拟 proxySubdomain 生成的地址）
	hexLabel := hex.EncodeToString([]byte("https://example.com"))
	valid := hexLabel + ".localhost:8765"
	if !isSubdomain(valid) {
		t.Fatalf("应识别为子域代理: %q", valid)
	}
	// bad 集：公网域名/裸域/非hex标签/带端口裸域
	for _, bad := range []string{"example.com", "localhost", "zzzz.localhost", "localhost:8765"} {
		if isSubdomain(bad) {
			t.Fatalf("不应识别为子域代理: %q", bad)
		}
	}
}

// TestDecodeSubdomainOrigin 校验从子域 Host 反解目标 origin。
//
// 作用：proxySubdomain 的第一步依赖它拿目标；解错=整个请求发错人。
// 场景：合法 hex 标签应解回 scheme+host；非法 hex 返回 nil。
// 断言点：解出的 Scheme/Host 正确；"nothex" 标签返回 nil。
func TestDecodeSubdomainOrigin(t *testing.T) {
	origin := "https://example.com"
	hexLabel := hex.EncodeToString([]byte(origin))
	got := decodeSubdomainOrigin(hexLabel + ".localhost:8765")
	if got == nil {
		t.Fatalf("解码子域 origin 失败")
	}
	if got.Scheme != "https" || got.Host != "example.com" {
		t.Fatalf("解码结果错误: scheme=%q host=%q", got.Scheme, got.Host)
	}
	// 非十六进制标签（非本代理生成）必须返回 nil，交回 UI 处理
	if decodeSubdomainOrigin("nothex.localhost:8765") != nil {
		t.Fatalf("非法 hex 应返回 nil")
	}
}

// TestInCIDR 校验网段包含判断与非法输入容错。
//
// 作用：classify 的黑/白名单全部网段判定最终都落到这个函数。
// 场景：在段内、段外、非法 CIDR 三种输入。
// 断言点：段内 true、段外 false、非法文本 false（配置写错不 panic）。
func TestInCIDR(t *testing.T) {
	ip := net.ParseIP("192.168.1.5")
	// 在 192.168.1.0/24 内
	if !inCIDR(ip, "192.168.1.0/24") {
		t.Fatalf("应在 CIDR 内")
	}
	// 不在 10.0.0.0/8 内
	if inCIDR(ip, "10.0.0.0/8") {
		t.Fatalf("不应在 CIDR 内")
	}
	// 非法 CIDR 文本返回 false（而非 panic）
	if inCIDR(ip, "not-a-cidr") {
		t.Fatalf("非法 CIDR 应返回 false")
	}
}

// TestSSRFClassify 校验 SSRF 分类器在两种模式下的行为。
//
// 作用：classify 是 SSRF 防护的判定内核，规则错=防护形同虚设。
// 场景：先用线上同款黑名单配置（拦 loopback/link-local、放行私网），
// 再切白名单配置验证"默认全拒、命中才放"。
// 断言点（黑名单部分）：
//   - 127.0.0.1 -> "loopback"；
//   - 169.254.169.254（云元数据）-> "linklocal"；
//   - 192.168.0.107（RFC1918）-> ""（默认放行，符合项目约定）；
//   - 0.0.0.0 -> "unspecified"；
//   - 打开 block_private 后 10.0.0.1 -> "private"。
//
// 断言点（白名单部分）：
//   - 192.168.0.107 在 allowed_ips 段内 -> ""；
//   - 10.0.0.1 既不在段内也不在域名表 -> "whitelist"；
//   - example.com 在 allowed_domains 中 -> ""（域名兜底放行）。
func TestSSRFClassify(t *testing.T) {
	// 黑名单配置：与 config.example.json 默认值一致
	cfg := &SSRFConfig{
		Mode:           "blacklist",
		BlockLoopback:  true,
		BlockLinkLocal: true,
		BlockPrivate:   false,
	}
	// 回环地址必须拦截
	if r := cfg.classify(net.ParseIP("127.0.0.1"), nil); r != "loopback" {
		t.Fatalf("loopback 未拦: %q", r)
	}
	// 云元数据（link-local）必须拦截
	if r := cfg.classify(net.ParseIP("169.254.169.254"), nil); r != "linklocal" {
		t.Fatalf("linklocal 未拦: %q", r)
	}
	// 内网 RFC1918（默认放行，这是"访问内网站点"的核心诉求）
	if r := cfg.classify(net.ParseIP("192.168.0.107"), nil); r != "" {
		t.Fatalf("RFC1918 不应被拦(默认): %q", r)
	}
	// 未指定地址
	if r := cfg.classify(net.ParseIP("0.0.0.0"), nil); r != "unspecified" {
		t.Fatalf("unspecified 未拦: %q", r)
	}

	// 开启 block_private 后应拦内网（验证开关本身生效）
	cfg.BlockPrivate = true
	if r := cfg.classify(net.ParseIP("10.0.0.1"), nil); r != "private" {
		t.Fatalf("private 未拦: %q", r)
	}

	// 白名单模式：仅放行 allowed_ips 段内或 allowed_domains 命中
	wcfg := &SSRFConfig{
		Mode:           "whitelist",
		AllowedIPs:     []string{"192.168.0.0/24"},
		AllowedDomains: []string{"example.com"},
	}
	// IP 命中白名单段 -> 放行
	if r := wcfg.classify(net.ParseIP("192.168.0.107"), mustURL("http://192.168.0.107")); r != "" {
		t.Fatalf("白名单 IP 应放行: %q", r)
	}
	// 两者都不命中 -> 拦
	if r := wcfg.classify(net.ParseIP("10.0.0.1"), mustURL("http://10.0.0.1")); r != "whitelist" {
		t.Fatalf("白名单外应拦: %q", r)
	}
	// IP 不在段内但域名在白名单 -> 域名兜底放行
	if r := wcfg.classify(net.ParseIP("93.184.216.34"), mustURL("http://example.com")); r != "" {
		t.Fatalf("白名单域名应放行: %q", r)
	}
}

// TestRewriteOriginInURL 校验 Referer 类头部的源站改写。
//
// 作用：doProxy 对 Referer 只换源不换路径（尽量保留可读的路径信息），
// 该函数是策略的唯一实现。
// 场景：绝对 URL 换源；相对值原样。
// 断言点：
//   - "http://localhost:8765/foo?a=1" -> "https://example.com/foo?a=1"
//     （scheme+host 换成目标，path/query 保留）；
//   - "/relative/path" 无 scheme/host，原样返回（相对引用改写反而失真）。
func TestRewriteOriginInURL(t *testing.T) {
	target, _ := url.Parse("https://example.com")
	got := rewriteOriginInURL("http://localhost:8765/foo?a=1", target)
	if got != "https://example.com/foo?a=1" {
		t.Fatalf("改写 Origin 错误: %q", got)
	}
	// 无法解析时原样返回
	if got := rewriteOriginInURL("/relative/path", target); got != "/relative/path" {
		t.Fatalf("相对值应原样返回: %q", got)
	}
}

// TestTruncate 校验日志截断函数。
//
// 作用：dbg 打印 Cookie/Authorization 等长值都要过它，规则很简单
// 但错一点日志就会刷屏或信息缺失。
// 场景：短串不截断、超长串截断并加尾标记。
// 断言点：len<=n 原样；len>n 得 s[:n]+"...[trunc]"。
func TestTruncate(t *testing.T) {
	// 短于上限：原样
	if truncate("hello", 10) != "hello" {
		t.Fatalf("短串不应截断")
	}
	// 超上限：截到 5 字符并追加标记
	if truncate("hello world", 5) != "hello...[trunc]" {
		t.Fatalf("长串截断错误: %q", truncate("hello world", 5))
	}
}

// mustURL 解析失败即 panic 的 URL 构造助手。
//
// 作用：测试里把字面量 URL 变成 *url.URL，失败时立刻炸掉而不是
// 带着 nil 走进断言，掩盖真实问题。
// 场景：TestSSRFClassify 等处批量构造目标 URL。
// 入参：
//   - s：URL 字面量。
//
// 出参：
//   - *url.URL：解析结果；解析失败直接 panic。
func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
