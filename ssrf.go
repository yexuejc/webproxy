// 本文件实现 SSRF 防护：在把请求转发给上游之前，对目标 URL 做协议与
// IP 分类检查，防止代理被滥用为"打内网/打云元数据"的跳板。
//
// 三种模式（config.json 的 ssrf.mode）：
//   - off：完全关闭（仅限可信环境自用，开源示例禁止使用）；
//   - blacklist：黑名单模式——默认拦截 loopback / link-local /
//     reserved，RFC1918 私网默认放行（block_private=false），
//     因为"经服务器访问其内网站点"正是本代理的核心场景；
//   - whitelist：白名单模式——只放行允许的协议/IP/域名，最严格。
package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// SSRFConfig 是 SSRF 防护的全部配置项（对应 config.json 的 ssrf 节）。
//
// 场景：由 main.go 启动时从 config.json 反序列化；之后每次代理请求
// 都以只读方式引用它。
//
// 字段说明：
//   - Mode：防护模式，"off" / "blacklist" / "whitelist"；空串按
//     blacklist 处理。
//   - BlockLoopback：是否拦截 127.0.0.0/8、::1 等回环地址。
//   - BlockLinkLocal：是否拦截 169.254.0.0/16（云元数据
//     169.254.169.254 所在段）与链路本地组播。
//   - BlockReserved：是否拦截特殊保留段（CGNAT 100.64/10、
//     IETF 192.0.0/24、基准测试 198.18/15、保留 240/4）。
//   - BlockPrivate：是否拦截 RFC1918 私网（10/8、172.16/12、
//     192.168/16）。本项目默认 false——访问内网站点是核心诉求，
//     不能一刀切禁私网。
//   - DeniedProtocols / DeniedIPs：黑名单的额外补充（blacklist
//     模式生效），例如禁止非 http/https 协议、禁止某段具体内网。
//   - AllowedProtocols / AllowedIPs / AllowedDomains：白名单条目
//     （whitelist 模式生效），三者是"或"的关系：命中任意一条即放行。
type SSRFConfig struct {
	Mode             string   `json:"mode"`
	BlockLoopback    bool     `json:"block_loopback"`
	BlockLinkLocal   bool     `json:"block_link_local"`
	BlockReserved    bool     `json:"block_reserved"`
	BlockPrivate     bool     `json:"block_private"`
	DeniedIPs        []string `json:"denied_ips"`
	DeniedProtocols  []string `json:"denied_protocols"`
	AllowedIPs       []string `json:"allowed_ips"`
	AllowedDomains   []string `json:"allowed_domains"`
	AllowedProtocols []string `json:"allowed_protocols"`
}

// resolveAndCheck 解析目标主机名，并对解析出的每个 IP 做分类检查。
//
// 作用：SSRF 防护的核心闸门——返回 nil error 才允许把请求发出去。
// 场景：doProxy 真正建立上游连接之前调用，在标准库 Transport 之外
// 先自查一遍，确保流量不会打到不该打的地址。
// 入参：
//   - target：目标 URL，取其 Scheme 与 Hostname 做协议/IP 检查。
//
// 出参：
//   - []net.IP：DNS 解析到的全部地址，调用方可直接复用以避免二次
//     解析（也可以选择忽略，让 Transport 自己再解析）。
//   - error：拦截时返回中文错误（含原因分类与 offending IP）；
//     off 模式下只可能是 DNS 解析失败。
func (c *SSRFConfig) resolveAndCheck(target *url.URL) ([]net.IP, error) {
	// 分支：off 模式 / 空 Mode —— 不做任何检查，仅解析 DNS 供调用方复用
	if c.Mode == "off" || c.Mode == "" {
		return net.LookupIP(target.Hostname())
	}
	// 协议统一按小写比较（配置里写 HTTP/https 都能匹配）
	scheme := strings.ToLower(target.Scheme)
	// 分支一：白名单模式 —— 协议必须命中 AllowedProtocols
	if c.Mode == "whitelist" {
		ok := false
		for _, p := range c.AllowedProtocols {
			if strings.ToLower(p) == scheme {
				ok = true
				break
			}
		}
		// 白名单未命中：直接拒绝，IP 检查都免了（协议层就出局）
		if !ok {
			return nil, fmt.Errorf("协议 %q 不在白名单", target.Scheme)
		}
	} else {
		// 分支二：黑名单模式 —— 协议只要不在 DeniedProtocols 里即可通过
		for _, p := range c.DeniedProtocols {
			if strings.ToLower(p) == scheme {
				return nil, fmt.Errorf("协议 %q 被禁止", target.Scheme)
			}
		}
	}

	// DNS 解析：把域名变成 IP 列表，后面逐个分类
	ips, err := net.LookupIP(target.Hostname())
	if err != nil {
		return nil, fmt.Errorf("DNS 解析失败: %w", err)
	}
	// 逐个 IP 分类。这是有意的保守策略：DNS 轮询/双栈主机可能同时
	// 返回公网与内网地址，只要有一个踩线就整体拒绝（防 DNS 回转）。
	for _, ip := range ips {
		if reason := c.classify(ip, target); reason != "" {
			return nil, fmt.Errorf("目标地址被拦截(%s): %s", reason, ip.String())
		}
	}
	return ips, nil
}

// classify 判定单个 IP 是否放行；空字符串表示放行，非空为拦截原因。
//
// 作用：把一个 IP 归入某个拦截类别（whitelist / loopback / ...）。
// 场景：由 resolveAndCheck 对 DNS 结果的每个 IP 调用；黑名单与白名单
// 两套逻辑都在这里分流。
// 入参：
//   - ip：待判定的 IP（v4 或 v6）。
//   - target：目标 URL，白名单模式判定域名后缀时需要其 Hostname。
//
// 出参：
//   - string：拦截原因（用于错误提示与日志）；"" 表示放行。
func (c *SSRFConfig) classify(ip net.IP, target *url.URL) string {
	// 分支一：白名单模式 —— 默认全拒，命中 AllowedIPs 或 AllowedDomains
	// 之一才放行
	if c.Mode == "whitelist" {
		matched := false
		// 先按 CIDR 逐条匹配 IP 段
		for _, cidr := range c.AllowedIPs {
			if inCIDR(ip, cidr) {
				matched = true
				break
			}
		}
		// IP 没命中时再试域名：host == d 精确匹配，或以 "."+d 结尾
		// （后者覆盖子域，如白名单填 runoob.com 时 www.runoob.com 也放行）
		if !matched {
			for _, d := range c.AllowedDomains {
				host := strings.ToLower(target.Hostname())
				d = strings.ToLower(d)
				if host == d || strings.HasSuffix(host, "."+d) {
					matched = true
					break
				}
			}
		}
		// IP 与域名都没命中：拦截。原因串统一为 "whitelist"，
		// 便于上游拼出可读错误。
		if !matched {
			return "whitelist"
		}
		// 命中任一白名单条目：放行（不再走下面的黑名单检查）
		return ""
	}
	// 分支二：黑名单模式 —— 默认全放，逐条踩线才拦。判断顺序从
	// 具体到宽泛：
	// 1) 回环地址：127.0.0.0/8、::1
	if c.BlockLoopback && ip.IsLoopback() {
		return "loopback"
	}
	// 2) 链路本地：169.254/16 与 ff02::/16 等。其中 169.254.169.254
	//    是云厂商元数据地址，泄露它等于泄露临时凭证，必须默认拦截。
	if c.BlockLinkLocal && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
		return "linklocal"
	}
	// 3) RFC1918 私网：10/8、172.16/12、192.168/16。本项默认关闭——
	//    "经服务器访问其内网站点"是核心场景，不能一刀切。
	if c.BlockPrivate && ip.IsPrivate() {
		return "private"
	}
	// 4) 未指定地址：0.0.0.0、:: —— 打过去没有意义且常被利用
	if ip.IsUnspecified() {
		return "unspecified"
	}
	// 5) 保留段：CGNAT 100.64/10、IETF 192.0.0/24、基准测试 198.18/15、
	//    将来使用 240/4。Go 标准库没有现成判断函数，逐段手判。
	if c.BlockReserved {
		if inCIDR(ip, "100.64.0.0/10") || inCIDR(ip, "192.0.0.0/24") ||
			inCIDR(ip, "198.18.0.0/15") || inCIDR(ip, "240.0.0.0/4") {
			return "reserved"
		}
	}
	// 6) 自定义黑名单：用户在 DeniedIPs 里填的具体网段
	for _, cidr := range c.DeniedIPs {
		if inCIDR(ip, cidr) {
			return "denied"
		}
	}
	// 全部检查通过：放行
	return ""
}

// inCIDR 判断 IP 是否落在给定 CIDR 网段内。
//
// 作用：把 "192.168.0.0/16" 这类文本配置变成可用的成员判断。
// 场景：classify 对 AllowedIPs / DeniedIPs / 内置保留段逐条调用。
// 入参：
//   - ip：待判定的 IP。
//   - cidr：CIDR 文本，如 "169.254.0.0/16"。
//
// 出参：
//   - bool：true 表示落在网段内；CIDR 文本非法时返回 false（配置写错
//     不应拖垮请求，按"不匹配"处理）。
func inCIDR(ip net.IP, cidr string) bool {
	// 解析 CIDR：非法配置（拼写错误等）时返回 false 而非 panic
	_, netC, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	return netC.Contains(ip)
}
