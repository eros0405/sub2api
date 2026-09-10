package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// 上游 5xx 错误率熔断。
//
// 背景：账号被上游单独软风控时，表现是 5xx 与成功交替出现（实测错误率 30%-100%），
// 而不是连续硬失败。已有的 openAIAccountModelTransientState 用连败计数判定，
// 一次成功就把 streak 清零，因此对这种概率性劣化天然免疫——线上曾出现
// 6 小时 6514 条 503、该熔断触发 0 次的情况。
//
// 本熔断改用滑动窗口内的错误率判定，粒度是**账号**而非 account+model：
// 软风控是账号级降级，失败会散落在多个模型上，按 account+model 分桶会进一步稀释样本。
//
// 计数在内存滑动，只在判定熔断的那一刻落库（temp_unschedulable_until），
// 因此写库频率等于熔断次数而非错误次数。

const (
	// upstream5xxBreakerStreakTTLFloor 是退避档位的最短保留时间。
	// 实际 TTL 取 max(该值, 3*MaxCooldown)，确保账号在回避期满后
	// 仍有足够时间证明自己健康，否则档位会在下一次熔断前就被清掉、退避失效。
	upstream5xxBreakerStreakTTLFloor = 30 * time.Minute

	// upstream5xxHealthyCountCacheTTL 是 MinHealthyAccounts 保底计数的缓存时长。
	// 大面积过载时多个账号会在同一瞬间判定熔断，若每次都查库会产生查询风暴。
	upstream5xxHealthyCountCacheTTL = 10 * time.Second
)

// upstream5xxWindow 记录单账号在滑动窗口内的成功/失败计数。
type upstream5xxWindow struct {
	mu        sync.Mutex
	start     time.Time
	failures  int
	successes int
}

// upstream5xxBreakerState 保存单账号的退避档位与最近一次熔断时间。
type upstream5xxBreakerState struct {
	mu         sync.Mutex
	tripStreak int
	lastTripAt time.Time
}

// upstream5xxHealthyCount 缓存同平台可调度账号数。
type upstream5xxHealthyCount struct {
	mu        sync.Mutex
	count     int
	fetchedAt time.Time
}

// observeUpstream5xxOutcome 在滑动窗口内累计一次结果，返回累计后的窗口快照。
// 窗口过期时先归零再计入，因此返回值总是当前窗口的真实计数。
func (s *OpenAIGatewayService) observeUpstream5xxOutcome(accountID int64, failure bool, window time.Duration, now time.Time) (failures int, total int) {
	if s == nil || accountID <= 0 || window <= 0 {
		return 0, 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	value, _ := s.openaiUpstream5xxWindows.LoadOrStore(accountID, &upstream5xxWindow{start: now})
	w, ok := value.(*upstream5xxWindow)
	if !ok {
		w = &upstream5xxWindow{start: now}
		s.openaiUpstream5xxWindows.Store(accountID, w)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	// 时钟回拨也重置，避免 now.Sub(start) 为负导致窗口永不过期。
	if now.Sub(w.start) >= window || now.Before(w.start) {
		w.start = now
		w.failures = 0
		w.successes = 0
	}
	if failure {
		w.failures++
	} else {
		w.successes++
	}
	return w.failures, w.failures + w.successes
}

// resetUpstream5xxWindow 清空窗口计数，用于熔断生效后避免同一窗口反复判定。
func (s *OpenAIGatewayService) resetUpstream5xxWindow(accountID int64, now time.Time) {
	if s == nil {
		return
	}
	value, ok := s.openaiUpstream5xxWindows.Load(accountID)
	if !ok {
		return
	}
	w, ok := value.(*upstream5xxWindow)
	if !ok {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	w.mu.Lock()
	w.start = now
	w.failures = 0
	w.successes = 0
	w.mu.Unlock()
}

// nextUpstream5xxCooldown 计算本次回避时长并推进退避档位。
// 冷却 = clamp(base * 2^streak, base, max)。max<=base 时恒定 base、不推进档位。
// 档位超过 TTL 未被使用则先归零，使长期健康的账号回到初始档位。
func (s *OpenAIGatewayService) nextUpstream5xxCooldown(accountID int64, settings *Upstream5xxBreakerSettings, now time.Time) time.Duration {
	base := time.Duration(settings.CooldownSeconds) * time.Second
	if base <= 0 {
		base = time.Duration(DefaultUpstream5xxBreakerSettings().CooldownSeconds) * time.Second
	}
	maxCooldown := time.Duration(settings.MaxCooldownSeconds) * time.Second
	if maxCooldown <= base {
		return base
	}

	streakTTL := 3 * maxCooldown
	if streakTTL < upstream5xxBreakerStreakTTLFloor {
		streakTTL = upstream5xxBreakerStreakTTLFloor
	}

	value, _ := s.openaiUpstream5xxBreakerStates.LoadOrStore(accountID, &upstream5xxBreakerState{})
	st, ok := value.(*upstream5xxBreakerState)
	if !ok {
		st = &upstream5xxBreakerState{}
		s.openaiUpstream5xxBreakerStates.Store(accountID, st)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.lastTripAt.IsZero() && (now.Sub(st.lastTripAt) > streakTTL || now.Before(st.lastTripAt)) {
		st.tripStreak = 0
	}
	cooldown := base
	for i := 0; i < st.tripStreak && cooldown < maxCooldown; i++ {
		cooldown *= 2
	}
	if cooldown > maxCooldown {
		cooldown = maxCooldown
	}
	st.tripStreak++
	st.lastTripAt = now
	return cooldown
}

// resetUpstream5xxBreakerState 在账号被证明健康后清除退避档位与窗口计数。
func (s *OpenAIGatewayService) resetUpstream5xxBreakerState(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.openaiUpstream5xxBreakerStates.Delete(accountID)
	s.openaiUpstream5xxWindows.Delete(accountID)
}

// upstream5xxBreakerSettings 读取熔断配置，settingService 缺失时返回 nil（调用方跳过熔断）。
func (s *OpenAIGatewayService) upstream5xxBreakerSettings(ctx context.Context) *Upstream5xxBreakerSettings {
	if s == nil || s.settingService == nil {
		return nil
	}
	settings, err := s.settingService.GetUpstream5xxBreakerSettings(ctx)
	if err != nil || settings == nil {
		return nil
	}
	return settings
}

// hasUpstream5xxHealthyHeadroom 判断摘掉当前账号后同平台是否还留有足够可调度账号。
// 计数带短 TTL 缓存，避免大面积过载时的查询风暴。
// 查询失败时返回 true（放行熔断）：保底的目的是防止误摘全池，
// 不应因为一次查询失败就让熔断整体失效。
func (s *OpenAIGatewayService) hasUpstream5xxHealthyHeadroom(ctx context.Context, platform string, minHealthy int, now time.Time) bool {
	if minHealthy <= 0 {
		return true
	}
	if s == nil || s.accountRepo == nil {
		return true
	}

	value, _ := s.openaiUpstream5xxHealthyCounts.LoadOrStore(platform, &upstream5xxHealthyCount{})
	c, ok := value.(*upstream5xxHealthyCount)
	if !ok {
		c = &upstream5xxHealthyCount{}
		s.openaiUpstream5xxHealthyCounts.Store(platform, c)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetchedAt.IsZero() || now.Sub(c.fetchedAt) >= upstream5xxHealthyCountCacheTTL || now.Before(c.fetchedAt) {
		count, err := s.accountRepo.CountSchedulableByPlatform(ctx, platform)
		if err != nil {
			return true
		}
		c.count = count
		c.fetchedAt = now
	}
	// 当前账号此刻仍计入可调度数，摘掉后剩余为 count-1。
	return c.count-1 >= minHealthy
}

// maybeTripUpstream5xxBreaker 记录一次上游 5xx，并在窗口错误率越界时熔断该账号。
// 返回是否触发了熔断。
func (s *OpenAIGatewayService) maybeTripUpstream5xxBreaker(ctx context.Context, account *Account, statusCode int, model string) bool {
	if s == nil || account == nil || account.ID <= 0 {
		return false
	}
	settings := s.upstream5xxBreakerSettings(ctx)
	if settings == nil || !settings.Enabled {
		return false
	}
	// Spark 影子账号不参与账号级熔断，与 429 路径保持一致。
	if account.IsShadow() {
		return false
	}

	now := time.Now()
	window := time.Duration(settings.WindowSeconds) * time.Second
	failures, total := s.observeUpstream5xxOutcome(account.ID, true, window, now)
	if total < settings.MinSamples {
		return false
	}
	rate := failures * 100 / total
	if rate < settings.ErrorRatePercent {
		return false
	}
	if !s.hasUpstream5xxHealthyHeadroom(ctx, account.Platform, settings.MinHealthyAccounts, now) {
		slog.Warn("openai_upstream_5xx_breaker_skipped_headroom",
			"account_id", account.ID,
			"platform", account.Platform,
			"error_rate", rate,
			"min_healthy_accounts", settings.MinHealthyAccounts)
		return false
	}

	cooldown := s.nextUpstream5xxCooldown(account.ID, settings, now)
	until := now.Add(cooldown)
	reason := upstream5xxBreakerReason(rate, settings.WindowSeconds, failures, total)

	// 内存标记让本实例立刻生效，不等调度快照轮转。
	s.BlockAccountScheduling(account, until, "upstream_5xx_breaker")
	// 落库让回避跨重启、跨实例可见；SetTempUnschedulable 自带
	// "新 until 晚于当前值才更新" 的单调守卫，并负责 outbox 入队与快照同步。
	if s.accountRepo != nil {
		if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
			slog.Warn("openai_upstream_5xx_breaker_persist_failed",
				"account_id", account.ID,
				"error", err)
		}
	}
	// 清空窗口，避免残留计数在回避结束后立即再次触发。
	s.resetUpstream5xxWindow(account.ID, now)

	slog.Warn("openai_upstream_5xx_breaker_tripped",
		"account_id", account.ID,
		"platform", account.Platform,
		"model", model,
		"status_code", statusCode,
		"error_rate", rate,
		"failures", failures,
		"samples", total,
		"window_seconds", settings.WindowSeconds,
		"cooldown_seconds", int(cooldown.Seconds()),
		"until", until.Format(time.RFC3339))
	return true
}

// observeUpstream5xxSuccess 记录一次成功。窗口内样本充足且错误率已回落到阈值以下时，
// 视为账号已恢复健康，清除退避档位使下次熔断回到初始档位。
func (s *OpenAIGatewayService) observeUpstream5xxSuccess(ctx context.Context, account *Account) {
	if s == nil || account == nil || account.ID <= 0 {
		return
	}
	settings := s.upstream5xxBreakerSettings(ctx)
	if settings == nil || !settings.Enabled {
		return
	}
	if account.IsShadow() {
		return
	}

	now := time.Now()
	window := time.Duration(settings.WindowSeconds) * time.Second
	failures, total := s.observeUpstream5xxOutcome(account.ID, false, window, now)
	if total < settings.MinSamples {
		return
	}
	if failures*100/total < settings.ErrorRatePercent {
		s.openaiUpstream5xxBreakerStates.Delete(account.ID)
	}
}

// upstream5xxBreakerReason 生成可读的 temp_unschedulable_reason。
// 前缀独立，避免与 "token refresh retry exhausted:" 等已有前缀的匹配逻辑相撞。
func upstream5xxBreakerReason(rate int, windowSeconds int, failures int, total int) string {
	return fmt.Sprintf("upstream 5xx breaker: rate=%d%% window=%ds failures=%d/%d",
		rate, windowSeconds, failures, total)
}
