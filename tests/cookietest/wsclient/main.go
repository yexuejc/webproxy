//go:build cookietest

// wsclient.go：cookietest 的 WebSocket 探针客户端。
//
// 作用：以裸 TCP 完成一次 WebSocket 握手，读出服务端回显的数据帧，
// 用来验证"经 webproxy 的 WS 隧道是否携带了正确的会话 Cookie"。
//
// 为什么不用现成库：要精确控制握手请求里 Cookie 头的有无/取值，
// 只有裸写请求才能做到；gorilla/websocket 等客户端会自行管理
// jar，反而遮住了被测行为。
//
// 用法：go run -tags cookietest ./tests/cookietest/wsclient <host:port> <cookie值>
//   - 第一个参数：代理子域地址，如 687474...localhost:18765；
//   - 第二个参数：期望注入的 Cookie 值（给 SECRET 验证注入，给
//     任意错值验证不带 Cookie 时的回显为空）。
//
// 判据：标准输出 "WS server frame: cookie=SECRET" 即注入成功；
// 带空/错 Cookie 时应得到空值（证明浏览器侧确实未携带）。
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
)

func main() {
	// 入参 1：目标 host:port（代理 WS 入口，含端口）
	hostPort := os.Args[1] // e.g. 687474...localhost:18765
	// 入参 2：要携带的 Cookie 值（测试脚本用 SECRET 验证注入）
	cookie := os.Args[2]
	// 拨号到目标（代理 -> 上游的链路由代理负责，这里只连代理）
	conn, err := net.Dial("tcp", hostPort)
	// 分支：连不上——打印原因并失败退出
	if err != nil {
		fmt.Println("dial err:", err)
		os.Exit(1)
	}
	defer conn.Close()
	// RFC 6455 示例用的标准 nonce（协议文档里现成的 Key）
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	// 手工拼升级请求：Host 带端口；Upgrade/Connection 必备；
	// Cookie 显式带上被测值
	req := "GET /websocket HTTP/1.1\r\n" +
		"Host: " + hostPort + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Cookie: fnos_sid=" + cookie + "\r\n" +
		"\r\n"
	conn.Write([]byte(req))
	// 读应答
	br := bufio.NewReader(conn)
	// 先读响应头直到空行（不解析内容，只消费掉）
	for {
		line, e := br.ReadString('\n')
		// 分支：读取中断——视为握手失败
		if e != nil {
			fmt.Println("read err:", e)
			os.Exit(1)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	// 读一个数据帧的前两字节（帧头）
	b0, _ := br.ReadByte() // b0 含 FIN/opcode，本测试不关心
	b1, _ := br.ReadByte() // b1 含 payload 长度（mask 位为 0：服务端帧不掩码）
	_ = b0
	length := int(b1 & 0x7f) // 取低 7 位长度（<126 的短帧）
	// 按长度读出 payload 并打印——mock 端回显的就是 Cookie 值
	buf := make([]byte, length)
	br.Read(buf)
	fmt.Printf("WS server frame: %s\n", string(buf))
}
