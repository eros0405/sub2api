package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
)

// PanelIPWhitelistSettings 面板 IP 白名单配置。
// 管理员动态维护；白名单为空或开关关闭时放行所有来源。
// 仅作用于面板（管理后台/用户面板页面及其 /api/v1 面板接口），
// /v1 网关代理与支付回调不受影响。
type PanelIPWhitelistSettings struct {
	// Enabled 总开关；关闭时无论白名单是否为空都放行
	Enabled bool `json:"enabled"`
	// Whitelist IP/CIDR 列表，每行一条；非空且 Enabled 时仅允许命中来源
	Whitelist []string `json:"whitelist"`
}

const (
	panelIPWhitelistCacheTTL  = 60 * time.Second
	panelIPWhitelistErrorTTL  = 5 * time.Second
	panelIPWhitelistDBTimeout = 5 * time.Second
)

// cachedPanelIPWhitelistSettings 进程内缓存条目（60s TTL），含预编译白名单规则。
type cachedPanelIPWhitelistSettings struct {
	settings  PanelIPWhitelistSettings
	compiled  *ip.CompiledIPRules
	expiresAt int64 // unix nano
}

// DefaultPanelIPWhitelistSettings 返回默认面板 IP 白名单配置（关闭，放行所有）。
func DefaultPanelIPWhitelistSettings() *PanelIPWhitelistSettings {
	return &PanelIPWhitelistSettings{Enabled: false, Whitelist: []string{}}
}

// normalizePanelIPWhitelistSettings 去除条目首尾空白与空行。
func normalizePanelIPWhitelistSettings(s *PanelIPWhitelistSettings) {
	if s == nil {
		return
	}
	trimmed := make([]string, 0, len(s.Whitelist))
	for _, pattern := range s.Whitelist {
		pattern = strings.TrimSpace(pattern)
		if pattern != "" {
			trimmed = append(trimmed, pattern)
		}
	}
	s.Whitelist = trimmed
}

// GetPanelIPWhitelistSettings 获取面板 IP 白名单配置（直读 DB，供管理端读写路径使用）。
// 缺失/空/解析失败 → 返回默认配置（关闭）。
func (s *SettingService) GetPanelIPWhitelistSettings(ctx context.Context) (*PanelIPWhitelistSettings, error) {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyPanelIPWhitelistSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return DefaultPanelIPWhitelistSettings(), nil
		}
		return nil, fmt.Errorf("get panel ip whitelist settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return DefaultPanelIPWhitelistSettings(), nil
	}

	settings := &PanelIPWhitelistSettings{}
	if err := json.Unmarshal([]byte(value), settings); err != nil {
		slog.Warn("failed to unmarshal panel ip whitelist settings, falling back to defaults",
			"error", err, "key", SettingKeyPanelIPWhitelistSettings)
		return DefaultPanelIPWhitelistSettings(), nil
	}
	normalizePanelIPWhitelistSettings(settings)
	return settings, nil
}

// SetPanelIPWhitelistSettings 保存面板 IP 白名单配置，并立即刷新进程内缓存，
// 使当前节点的下一个请求即生效（多节点部署最迟 60s 内生效）。
func (s *SettingService) SetPanelIPWhitelistSettings(ctx context.Context, settings *PanelIPWhitelistSettings) error {
	if settings == nil {
		return fmt.Errorf("settings cannot be nil")
	}
	normalizePanelIPWhitelistSettings(settings)
	if invalid := ip.ValidateIPPatterns(settings.Whitelist); len(invalid) > 0 {
		return fmt.Errorf("invalid IP/CIDR patterns: %s", strings.Join(invalid, ", "))
	}

	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal panel ip whitelist settings: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyPanelIPWhitelistSettings, string(data)); err != nil {
		return err
	}

	s.storePanelIPWhitelistCache(*settings, panelIPWhitelistCacheTTL)
	return nil
}

// GetPanelIPWhitelistSettingsCached 返回面板 IP 白名单配置与预编译规则（进程内缓存，60s TTL）。
// 面板每个请求的热路径都会调用，绝不能每次访问 DB；
// DB 错误时返回最近一次已知值（无缓存则返回默认值，即放行），并以短 TTL 快速重试。
func (s *SettingService) GetPanelIPWhitelistSettingsCached(ctx context.Context) (PanelIPWhitelistSettings, *ip.CompiledIPRules) {
	if s == nil || s.settingRepo == nil {
		return *DefaultPanelIPWhitelistSettings(), ip.CompileIPRules(nil)
	}
	if cached, ok := s.panelIPWhitelistCache.Load().(*cachedPanelIPWhitelistSettings); ok && cached != nil {
		if time.Now().UnixNano() < cached.expiresAt {
			return cached.settings, cached.compiled
		}
	}

	result, _, _ := s.panelIPWhitelistSF.Do("panel_ip_whitelist_settings", func() (any, error) {
		// 二次检查，避免排队的 goroutine 重复查询
		if cached, ok := s.panelIPWhitelistCache.Load().(*cachedPanelIPWhitelistSettings); ok && cached != nil {
			if time.Now().UnixNano() < cached.expiresAt {
				return cached, nil
			}
		}
		if ctx == nil {
			ctx = context.Background()
		}
		// 独立 context：断开请求取消链，避免客户端断连污染缓存
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), panelIPWhitelistDBTimeout)
		defer cancel()

		settings, err := s.GetPanelIPWhitelistSettings(dbCtx)
		if err != nil {
			slog.Warn("failed to get panel ip whitelist settings", "error", err)
			// 保留最近一次已知值，短 TTL 快速重试
			fallback := *DefaultPanelIPWhitelistSettings()
			if prior, ok := s.panelIPWhitelistCache.Load().(*cachedPanelIPWhitelistSettings); ok && prior != nil {
				fallback = prior.settings
			}
			cached := &cachedPanelIPWhitelistSettings{
				settings:  fallback,
				compiled:  ip.CompileIPRules(fallback.Whitelist),
				expiresAt: time.Now().Add(panelIPWhitelistErrorTTL).UnixNano(),
			}
			s.panelIPWhitelistCache.Store(cached)
			return cached, nil
		}

		cached := &cachedPanelIPWhitelistSettings{
			settings:  *settings,
			compiled:  ip.CompileIPRules(settings.Whitelist),
			expiresAt: time.Now().Add(panelIPWhitelistCacheTTL).UnixNano(),
		}
		s.panelIPWhitelistCache.Store(cached)
		return cached, nil
	})
	if cached, ok := result.(*cachedPanelIPWhitelistSettings); ok && cached != nil {
		return cached.settings, cached.compiled
	}
	return *DefaultPanelIPWhitelistSettings(), ip.CompileIPRules(nil)
}

func (s *SettingService) storePanelIPWhitelistCache(settings PanelIPWhitelistSettings, ttl time.Duration) {
	s.panelIPWhitelistCache.Store(&cachedPanelIPWhitelistSettings{
		settings:  settings,
		compiled:  ip.CompileIPRules(settings.Whitelist),
		expiresAt: time.Now().Add(ttl).UnixNano(),
	})
}
