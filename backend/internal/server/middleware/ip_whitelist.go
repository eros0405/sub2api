package middleware

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// panelIPWhitelistExemptPrefixes 面板 IP 白名单不生效的路径前缀：
// /v1 网关代理（API Key 有自己的 IP 限制）、健康检查、支付回调（第三方服务器回调）。
var panelIPWhitelistExemptPrefixes = []string{
	"/v1",
	"/health",
	"/setup/status",
	"/api/event_logging/batch",
	"/api/v1/payment/webhook",
}

// PanelIPWhitelist 面板 IP 白名单中间件。
// 管理员在系统设置中动态维护白名单；非白名单 IP 访问面板（页面与 /api/v1 面板接口）
// 时返回 403：浏览器（Accept: text/html）返回简单「访问受限」HTML 页面，其余返回 JSON。
// 白名单为空或开关关闭时放行所有来源；配置改动后最迟 60s 生效（本节点立即生效）。
type PanelIPWhitelist struct {
	settingService *service.SettingService
}

// NewPanelIPWhitelist 创建面板 IP 白名单中间件。
func NewPanelIPWhitelist(settingService *service.SettingService) *PanelIPWhitelist {
	return &PanelIPWhitelist{settingService: settingService}
}

// Handler 返回 gin 中间件。
func (p *PanelIPWhitelist) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		for _, prefix := range panelIPWhitelistExemptPrefixes {
			if strings.HasPrefix(path, prefix) {
				c.Next()
				return
			}
		}

		settings, compiled := p.settingService.GetPanelIPWhitelistSettingsCached(c.Request.Context())
		if !settings.Enabled || len(settings.Whitelist) == 0 {
			c.Next()
			return
		}

		clientIP := SecurityClientIP(c)
		if allowed, _ := ip.CheckIPRestrictionWithCompiledRules(clientIP, compiled, nil); allowed {
			c.Next()
			return
		}

		MarkIngressRejected(c, IngressRejectIPRestricted)
		if strings.Contains(c.Request.Header.Get("Accept"), "text/html") {
			c.Data(http.StatusForbidden, "text/html; charset=utf-8", []byte(panelIPWhitelistDeniedPage))
			c.Abort()
			return
		}
		AbortWithError(c, http.StatusForbidden, "IP_NOT_ALLOWED", "Your IP address is not allowed to access this panel")
	}
}

// panelIPWhitelistDeniedPage 非白名单 IP 访问面板时展示的简单限制页面。
const panelIPWhitelistDeniedPage = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>403 - Access Denied</title>
<style>
body{margin:0;display:flex;align-items:center;justify-content:center;min-height:100vh;
font-family:system-ui,-apple-system,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif;
background:#f5f6f8;color:#1f2328}
.card{text-align:center;padding:48px 40px;background:#fff;border-radius:12px;
box-shadow:0 2px 12px rgba(0,0,0,.08);max-width:420px}
.code{font-size:64px;font-weight:700;color:#d93025;line-height:1}
h1{font-size:18px;margin:16px 0 8px}
p{font-size:14px;color:#57606a;margin:0}
</style>
</head>
<body>
<div class="card">
<div class="code">403</div>
<h1>访问受限 / Access Denied</h1>
<p>您的 IP 地址不在允许访问的列表中，如有疑问请联系管理员。</p>
<p>Your IP address is not allowed to access this panel.</p>
</div>
</body>
</html>`
