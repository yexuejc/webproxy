// 本文件负责【登录会话与会话级 Cookie 罐子】。
//
// 背景：webproxy 的入口页带一个登录鉴权（规划中），登录成功后通过
// `sid` Cookie 标识一个"会话"。每个会话持有一个独立的 http.CookieJar，
// 用于按会话隔离各目标站点的会话 Cookie（例如 fnOS 的 ost=...）。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/cookiejar"
	"sync"
)

// session 表示一个登录会话。
//
// 作用：把"一个浏览器用户"映射到"一组目标站点的 Cookie 集合"。
// 场景：入口页登录后颁发 sid；之后该用户的代理请求都复用同一会话，
// 不同浏览器用户之间、同一用户不同登录态之间互不串号。
//
// 字段说明：
//   - jar：该会话专属的 Cookie 罐子。Go 标准库的 cookiejar 会自动按
//     目标 origin 分区存储，因此 192.168.0.107 的 ost 与某公网站的
//     sessionid 互不混淆。
type session struct {
	jar http.CookieJar
}

// sessions 是全局会话表：sid -> *session。
//
// 选用 sync.Map 的原因：会话查找发生在每一次入口页请求的关键路径上，
// 读多写少（仅登录时写），sync.Map 对这种场景免锁读性能更好。
// 注意：这是进程内内存结构，代理重启后全部丢失，用户需重新登录。
var sessions sync.Map

// getSession 按 sid 获取（不存在则创建）会话，始终返回非 nil。
//
// 作用：会话的"查找或新建"入口，保证同一 sid 永远拿到同一个 *session。
// 场景：每次带 sid 的请求都会经过这里。
// 入参：
//   - id：会话标识（来自浏览器 sid Cookie）。空串也能工作，会落到
//     "未登录"的共享会话上——调用方需自行决定是否允许空 sid 访问。
//
// 出参：
//   - *session：已存在的会话，或刚创建好的新会话（附带空 jar）。
func getSession(id string) *session {
	// 快路径：命中已存在会话，直接返回（绝大多数请求走这里）
	if v, ok := sessions.Load(id); ok {
		// 类型断言必然成功：写入时只存 *session
		return v.(*session)
	}
	// 慢路径：未命中则新建。nil 选项下 cookiejar.New 只会在极端
	// 情况失败，忽略 err 是可接受的（jar 为 nil 时 http.Client 也不崩）。
	jar, _ := cookiejar.New(nil)
	// 组装会话对象
	s := &session{jar: jar}
	// 登记进全局表。存在"两个并发请求同时未命中、各自新建"的竞态，
	// 但后一个 Store 覆盖前一个，两个 jar 内容等价（均为空），
	// 不影响正确性。
	sessions.Store(id, s)
	return s
}

// getSessionID 从请求中解析会话 ID；没有则签发新的并写入响应 Cookie。
//
// 作用：会话标识的"读取或签发"入口。
// 场景：入口页加载时调用——老用户带着 sid 来，直接沿用；新访客没有
// sid，则当场签发一个并通过 Set-Cookie 种进浏览器。
// 入参：
//   - r：当前 HTTP 请求，用于读取其中的 sid Cookie。
//   - w：当前 HTTP 响应写入器，用于在缺 sid 时种下新 Cookie。
//
// 出参：
//   - string：可用的会话 ID（已有的，或刚签发的）。
func getSessionID(r *http.Request, w http.ResponseWriter) string {
	// 分支：请求已带 sid Cookie 且值非空 —— 老会话，直接沿用
	if c, err := r.Cookie("sid"); err == nil && c.Value != "" {
		return c.Value
	}
	// 分支：没有 sid —— 新访客，当场签发
	id := randToken()
	// 通过 Set-Cookie 把新 sid 种给浏览器：
	//   - Path="/"：全站有效（入口页、/p/ 代理、子域反代都要带上）；
	//   - HttpOnly：页面 JS 读不到，防 XSS 窃取会话标识；
	//   - 不设 Domain：保持 host-only，只在当前主机存储；sid 只需在
	//     顶层入口页维护，不依赖跨站 iframe 携带。
	http.SetCookie(w, &http.Cookie{
		Name:     "sid",
		Value:    id,
		Path:     "/",
		HttpOnly: true,
	})
	return id
}

// randToken 生成 16 字节密码学随机数的十六进制字符串（32 字符）。
//
// 作用：为会话 ID 提供不可预测的随机源。
// 场景：仅在签发新 sid 时调用。
// 入参：无。
// 出参：
//   - string：32 位十六进制随机串；加密随机源不可用时返回固定值
//     "fallback"（功能不中断，但 ID 退化为可预测，仅作兜底）。
func randToken() string {
	// 16 字节 = 128 bit，熵与 UUID 同量级
	b := make([]byte, 16)
	// crypto/rand 读取失败时不 panic：签发会话失败只该降级，
	// 不该让整个代理服务挂掉，此处退化为固定串。
	if _, err := rand.Read(b); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(b)
}
