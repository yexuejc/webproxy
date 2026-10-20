// cjprobe 是一次性的诊断小程序（package main，非测试文件）。
//
// 用途：验证 Go 标准库 cookiejar 对"Set-Cookie 带 Domain=IP"的
// 处理行为。历史背景（2026-10-20 排查 fnOS 登录问题时）：
// 曾怀疑 cookiejar 拒绝存储 IP 域名的 Cookie 导致登录态丢失，
// 本程序用最小复现证明：Domain 属性写 IP（如 192.168.0.107）时
// RFC 6265 判定为非法 Domain（IP 不允许出现在 Domain 属性中），
// 浏览器与 cookiejar 都会拒绝该 Cookie——问题在目标站的
// Domain=IP 写法，而非代理逻辑。真正根因（跨站 iframe SameSite
// 扣发）由后续 cookietest 坐实，见 tests/cookietest/。
//
// 运行：go run ./tests/cjprobe （无输出依赖，exit 0 即行为确认）
package main

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
)

func main() {
	// 模拟 fnOS：设置带 Domain=IP 的会话 Cookie
	// 这正是当初怀疑的"非法用法"——Domain 属性指向裸 IP
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /login：下发会话 Cookie（host-only 场景本不该带 Domain，
		// 这里故意带上以复现问题）
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{
				Name: "fnos_sid", Value: "SECRET",
				Domain: "192.168.0.107", Path: "/", HttpOnly: true,
			})
			w.WriteHeader(200)
			return
		}
		// 其余路径（/app/token 等）：校验会话 Cookie
		c, err := r.Cookie("fnos_sid")
		// 分支：无 Cookie 或值不对——401（模拟 fnOS 的鉴权失败）
		if err != nil || c.Value != "SECRET" {
			w.WriteHeader(401)
			fmt.Fprintln(w, "no-session")
			return
		}
		// 分支：Cookie 正确——200
		w.WriteHeader(200)
		fmt.Fprintln(w, "ok token")
	}))
	// 服务生命周期跟进程绑定
	defer up.Close()

	// 新建客户端并挂 cookiejar（与 webproxy doProxy 的用法同构）
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	client := &http.Client{Jar: jar}

	// 步骤 1：登录——观察 jar 是否收下这个 Domain=IP 的 Cookie。
	// 若收下，说明 cookiejar 本身不挑 IP；收不下，才需要考虑改写。
	resp, _ := client.Post(up.URL+"/login", "text/plain", nil)
	fmt.Println("login status:", resp.StatusCode)
	resp.Body.Close()

	// 步骤 2：带 jar 访问受保护接口——若 jar 没存上 Cookie，
	// 这里会是 401。这是整个探针的判据。
	resp2, _ := client.Get(up.URL + "/app/token")
	body := make([]byte, 64)
	n, _ := resp2.Body.Read(body)
	resp2.Body.Close()
	fmt.Printf("app/token status: %d body: %q\n", resp2.StatusCode, string(body[:n]))
}
