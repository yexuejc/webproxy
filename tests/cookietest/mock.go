//go:build cookietest

// mock.go：cookie/会话机制的 E2E mock 目标站（模拟 fnOS）。
//
// 作用：以最简 HTTP 服务复刻 fnOS 的会话语义，供 cookietest 三连测：
//  1. POST /login  —— 200，并下发会话 Cookie；
//  2. GET  /app/token —— 无 Cookie 401，带正确 Cookie 200；
//  3. WS   /websocket —— 完成升级握手，并把收到的 Cookie 值以一帧
//     数据回显（用于验证代理是否在服务端注入了 jar 中的 Cookie）。
//
// 关键设定（与真实 fnOS 一致）：Cookie 为 host-only（无 Domain 属性）
// + HttpOnly + SameSite=Lax。这正是触发 Chrome 跨站 iframe 扣发的
// 形态，改回带 Domain 的旧写法会让测试失去意义。
//
// 构建运行：go run -tags cookietest ./tests/cookietest
// （默认监听 127.0.0.1:18080，由 tests/README.md 里的脚本驱动）
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
)

// computeAccept 计算 WebSocket 握手应答 Sec-WebSocket-Accept。
//
// 作用：RFC 6455 规定服务端须返回 base64(SHA1(key + 固定 GUID))。
// 场景：/websocket 升级握手时调用。
// 入参：
//   - key：客户端请求头 Sec-WebSocket-Key 的值。
//
// 出参：
//   - string：可直接写入 Sec-WebSocket-Accept 头的值。
func computeAccept(key string) string {
	// 固定 GUID 是协议规定值，不可更改
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

func main() {
	// /login：模拟 fnOS 的 /app/ticket 成功路径——下发会话 Cookie。
	// 注意：host-only（无 Domain）+ SameSite=Lax + HttpOnly，三项都要
	// 与真实 fnOS 对齐，否则测不出跨站 iframe 扣发问题。
	http.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		// 模拟真实 fnOS：host-only Cookie + SameSite=Lax（无 Domain 属性）
		http.SetCookie(w, &http.Cookie{
			Name: "ost", Value: "SECRET",
			Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":"ok"}`))
	})
	// /app/token：模拟 fnOS 的会话校验接口——有 Cookie 才放行
	http.HandleFunc("/app/token", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("ost")
		// 分支：缺 Cookie 或值不对——401（对应 fnOS 的登录态失效）
		if err != nil || c.Value != "SECRET" {
			w.WriteHeader(401)
			w.Write([]byte("invalid token"))
			return
		}
		// 分支：Cookie 正确——200 + 模拟 token 响应
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":"ok","token":"tk-123"}`))
	})
	// /websocket：手工完成 RFC 6455 升级握手，并把收到的 Cookie 回显
	http.HandleFunc("/websocket", func(w http.ResponseWriter, r *http.Request) {
		// Hijack 拿走底层 TCP 连接：net/http 之后不再介入，
		// 升级应答与数据帧都要手工写
		hj, _ := w.(http.Hijacker)
		conn, rw, err := hj.Hijack()
		// 分支：Hijack 失败——断开（该 mock 场景下视为测试环境异常）
		if err != nil {
			return
		}
		defer conn.Close()
		// 取握手 Key，用协议算法算 Accept
		key := r.Header.Get("Sec-WebSocket-Key")
		// 记下握手请求里的 Cookie 值——这就是"代理是否注入了 jar
		// 会话 Cookie"的观测点
		cookieVal := ""
		if ck, e := r.Cookie("ost"); e == nil {
			cookieVal = ck.Value
		}
		// 写 101 应答（三个必备头）
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
		rw.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
		rw.WriteString("Sec-WebSocket-Accept: " + computeAccept(key) + "\r\n\r\n")
		rw.Flush()
		// 以一帧文本数据把 Cookie 值回显给客户端：测试端读到的
		// "cookie=SECRET" 即证明服务端 jar 注入成功
		payload := []byte("cookie=" + cookieVal)
		rw.Write([]byte{0x81, byte(len(payload))}) // 0x81 = FIN+text opcode；载荷 <126 故长度直接进 b1
		rw.Write(payload)
		rw.Flush()
	})
	// 兜底页：便于浏览器/curl 直接验证 mock 存活
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><body>fnos mock</body></html>"))
	})
	// 只绑回环：mock 仅供本机测试，不应暴露到局域网
	fmt.Println("mock fnOS up on 127.0.0.1:18080")
	http.ListenAndServe("127.0.0.1:18080", nil)
}
