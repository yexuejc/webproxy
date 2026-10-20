// 本文件是 webproxy 的入口：负责加载配置、装配全局变量、注册路由并
// 启动 HTTP 服务。无任何第三方依赖初始化，单二进制即可运行。
package main

import (
	"encoding/json"
	"net/http"
	"os"
)

// Config 是整机配置结构（对应运行目录下的 config.json）。
//
// 场景：main() 启动时尝试读取 config.json 并反序列化到该结构；读取
// 失败（如全新 clone 没有该文件）时使用内置安全默认值。
//
// 字段说明：
//   - Listen：HTTP 监听地址，如 ":8765"。
//   - BasePath：base64 路径代理的前缀，默认 "/p/"。
//   - InsecureSkipVerify：向上游发起 TLS 时是否跳过证书校验
//     （访问自签证书的内网站点时开，生产慎用）。
//   - SSRF：SSRF 防护配置，详见 ssrf.go 的 SSRFConfig。
type Config struct {
	Listen             string     `json:"listen"`
	BasePath           string     `json:"base_path"`
	InsecureSkipVerify bool       `json:"insecure_skip_verify"`
	SSRF               SSRFConfig `json:"ssrf"`
}

// cfg 是全局配置副本：main() 装载后不再修改，各 handler 只读引用。
var cfg Config

// ssrfCfg 是 SSRF 配置的便捷别名：每次代理请求前都要查，抽成全局
// 变量避免层层传参；与 cfg.SSRF 指向同一份数据。
var ssrfCfg SSRFConfig

// basePath 是 base64 路径代理前缀（形如 "/p/"），全局只读。
// rewriter.go / proxy.go 拼代理路径时都从这里取，保证入口一致。
var basePath string

// main 程序入口：加载配置 -> 规范化参数 -> 注册路由 -> 阻塞式监听。
//
// 作用：完成服务启动的全部装配工作。
// 场景：二进制启动时调用一次；进程存续期内不返回。
// 入参：无。
// 出参：无（ListenAndServe 正常时永久阻塞；异常时直接退出进程）。
func main() {
	// 尝试读取运行目录下的 config.json
	if data, err := os.ReadFile("config.json"); err == nil {
		// 读到了：解析 JSON，失败则 panic——配置写错属于启动期
		// 致命错误，带病运行比直接挂掉更危险。
		if e := json.Unmarshal(data, &cfg); e != nil {
			panic("config.json 解析失败: " + e.Error())
		}
	} else {
		// 分支：没有 config.json（全新 clone / 最小部署）——用内置
		// 安全默认值兜底。注意 SSRF 默认 blacklist 且禁用危险协议，
		// 绝不默认 off；私网默认放行（block_private=false），
		// 因为经服务器访问其内网站点是本项目的核心场景。
		cfg.Listen = ":8765"
		cfg.BasePath = "/p/"
		cfg.SSRF = SSRFConfig{
			Mode:            "blacklist",
			BlockLoopback:   true,
			BlockLinkLocal:  true,
			BlockReserved:   false,
			BlockPrivate:    false,
			DeniedProtocols: []string{"file", "gopher", "ftp", "ftps", "dict", "tftp", "ldap", "sftp"},
		}
	}
	// 把 BasePath 抄送到全局变量，随后统一规范化
	basePath = cfg.BasePath
	// 分支：未配置则回落默认前缀
	if basePath == "" {
		basePath = "/p/"
	}
	// 分支：配置没带前导斜杠则补上
	if basePath[0] != '/' {
		basePath = "/" + basePath
	}
	// 分支：配置没带结尾斜杠则补上（保证拼接时 PREFIX+enc 不会粘连）
	if basePath[len(basePath)-1] != '/' {
		basePath += "/"
	}
	// SSRF 配置同样抄送一份便捷别名
	ssrfCfg = cfg.SSRF
	// 空 Mode 按黑名单处理（与无配置文件时的默认行为对齐）
	if ssrfCfg.Mode == "" {
		ssrfCfg.Mode = "blacklist"
	}
	// 白名单协议为空时默认只放 http/https，防止配置遗漏导致全协议放行
	if len(ssrfCfg.AllowedProtocols) == 0 {
		ssrfCfg.AllowedProtocols = []string{"http", "https"}
	}

	// 注册根路由。注意三个入口（根 UI、/p/ 前缀、子域名反代）都在
	// 根模式上分流：rootHandler（见 proxy.go）按 Host/Path 特征分发到
	// uiHandler / base64 路径代理 / 子域名反代。
	http.HandleFunc("/", rootHandler)
	// 阻塞式启动；返回错误（如端口占用）时直接忽略退出码，
	// 依赖日志/进程退出即可（单二进制极简风格）。
	http.ListenAndServe(cfg.Listen, nil)
}

// uiHandler 提供入口 UI 页面（static/index.html）。
//
// 作用：给浏览器返回"输入 URL 即可代理"的首页。
// 场景：用户直接访问 http://<主机>:<端口>/ 时由 rootHandler 分流进来。
// 入参：
//   - w：响应写入器。
//   - r：当前请求；本函数只用其 URL.Path。
//
// 出参：无（直接写响应；异常路径写 404/500 状态码）。
func uiHandler(w http.ResponseWriter, r *http.Request) {
	// 分支：favicon.ico —— 浏览器访问任何页面都会自动拉图标，
	// 返回 204 比 404 更干净，避免日志与 Network 面板噪声。
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// 分支：非根路径 —— UI 只服务 "/"，其余一律 404
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// 读首页文件。static/ 目录随二进制分发，缺失属于部署问题
	// （例如直接从模块目录外运行），返回 500 并说明原因。
	data, err := os.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "UI 文件 static/index.html 不存在", 500)
		return
	}
	// 显式声明 UTF-8：页面内含中文文案，省略 charset 会依赖浏览器
	// 嗅探，个别环境下会乱码。
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}
