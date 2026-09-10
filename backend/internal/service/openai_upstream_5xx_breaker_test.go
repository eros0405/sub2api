package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type upstream5xxAccountRepo struct {
	AccountRepository
	setCalls         int
	lastUntil        time.Time
	lastReason       string
	schedulableCount int
	schedulableErr   error
	schedulableCalls int
}

func (r *upstream5xxAccountRepo) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.setCalls++
	r.lastUntil = until
	r.lastReason = reason
	return nil
}

func (r *upstream5xxAccountRepo) CountSchedulableByPlatform(context.Context, string) (int, error) {
	r.schedulableCalls++
	if r.schedulableErr != nil {
		return 0, r.schedulableErr
	}
	return r.schedulableCount, nil
}

func newUpstream5xxService(t *testing.T, settings *Upstream5xxBreakerSettings, repo AccountRepository) *OpenAIGatewayService {
	t.Helper()
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	return &OpenAIGatewayService{
		accountRepo: repo,
		settingService: &SettingService{
			settingRepo: &fakeSettingRepo{vals: map[string]string{
				SettingKeyUpstream5xxBreakerSettings: string(raw),
			}},
		},
	}
}

func upstream5xxTestAccount() *Account {
	return &Account{ID: 77, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
}

func upstream5xxTestSettings() *Upstream5xxBreakerSettings {
	s := DefaultUpstream5xxBreakerSettings()
	s.Enabled = true
	s.MinSamples = 10
	s.ErrorRatePercent = 40
	s.CooldownSeconds = 60
	s.MaxCooldownSeconds = 480
	s.MinHealthyAccounts = 0
	return s
}

// 核心回归：5xx 与成功交替出现时必须熔断。
// 这正是旧的连败式熔断（openAIAccountModelTransientState）漏掉的场景——
// 每次成功都会把 streak 清零，导致线上 6 小时 6514 条 503 触发 0 次。
func TestUpstream5xxBreakerTripsOnAlternatingFailures(t *testing.T) {
	repo := &upstream5xxAccountRepo{schedulableCount: 100}
	svc := newUpstream5xxService(t, upstream5xxTestSettings(), repo)
	account := upstream5xxTestAccount()
	ctx := context.Background()

	tripped := false
	// 交替 6 失败 / 6 成功 = 50% 错误率，超过 40% 阈值。
	for i := 0; i < 6; i++ {
		svc.observeUpstream5xxSuccess(ctx, account)
		if svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.6-sol") {
			tripped = true
		}
	}

	require.True(t, tripped, "alternating success/failure at 50%% error rate must trip the breaker")
	require.Equal(t, 1, repo.setCalls, "trip must persist to temp_unschedulable_until exactly once")
	require.Contains(t, repo.lastReason, "upstream 5xx breaker:")
}

func TestUpstream5xxBreakerRespectsMinSamples(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.MinSamples = 20
	repo := &upstream5xxAccountRepo{schedulableCount: 100}
	svc := newUpstream5xxService(t, settings, repo)
	account := upstream5xxTestAccount()
	ctx := context.Background()

	// 10 次全失败 = 100% 错误率，但样本不足 20，不应熔断。
	for i := 0; i < 10; i++ {
		require.False(t, svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.5"))
	}
	require.Zero(t, repo.setCalls)
}

func TestUpstream5xxBreakerRespectsErrorRateThreshold(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.MinSamples = 10
	settings.ErrorRatePercent = 60
	repo := &upstream5xxAccountRepo{schedulableCount: 100}
	svc := newUpstream5xxService(t, settings, repo)
	account := upstream5xxTestAccount()
	ctx := context.Background()

	// 4 失败 / 16 成功 = 20% 错误率，低于 60% 阈值。
	for i := 0; i < 16; i++ {
		svc.observeUpstream5xxSuccess(ctx, account)
	}
	for i := 0; i < 4; i++ {
		require.False(t, svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.5"))
	}
	require.Zero(t, repo.setCalls)
}

// 高流量健康账号即使绝对错误数不小，也不能被熔断。
// 这是"只用绝对次数判定"会误杀的场景：线上健康账号 5 分钟窗口内
// 曾攒到 8-9 个 503，而劣化账号最低只有 9-10 个，绝对值区间重叠。
func TestUpstream5xxBreakerDoesNotTripHighVolumeHealthyAccount(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.MinSamples = 20
	settings.ErrorRatePercent = 30
	repo := &upstream5xxAccountRepo{schedulableCount: 100}
	svc := newUpstream5xxService(t, settings, repo)
	account := upstream5xxTestAccount()
	ctx := context.Background()

	// 9 失败 / 300 成功 ≈ 2.9% 错误率。
	for i := 0; i < 300; i++ {
		svc.observeUpstream5xxSuccess(ctx, account)
	}
	for i := 0; i < 9; i++ {
		require.False(t, svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.6-sol"))
	}
	require.Zero(t, repo.setCalls, "healthy high-volume account must survive an absolute error burst")
}

func TestUpstream5xxBreakerDisabledDoesNothing(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.Enabled = false
	repo := &upstream5xxAccountRepo{schedulableCount: 100}
	svc := newUpstream5xxService(t, settings, repo)
	account := upstream5xxTestAccount()
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		require.False(t, svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.5"))
	}
	require.Zero(t, repo.setCalls)
}

// MinHealthyAccounts 保底：大面积过载时不能把整池摘光。
func TestUpstream5xxBreakerHonorsMinHealthyAccounts(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.MinSamples = 10
	settings.MinHealthyAccounts = 5
	// 摘掉当前账号后只剩 4 个，低于保底 5，不允许熔断。
	repo := &upstream5xxAccountRepo{schedulableCount: 5}
	svc := newUpstream5xxService(t, settings, repo)
	account := upstream5xxTestAccount()
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		require.False(t, svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.5"))
	}
	require.Zero(t, repo.setCalls)

	// 池子充裕时应正常熔断。
	repo2 := &upstream5xxAccountRepo{schedulableCount: 50}
	svc2 := newUpstream5xxService(t, settings, repo2)
	account2 := upstream5xxTestAccount()
	tripped := false
	for i := 0; i < 20; i++ {
		if svc2.maybeTripUpstream5xxBreaker(ctx, account2, 503, "gpt-5.5") {
			tripped = true
			break
		}
	}
	require.True(t, tripped)
	require.Equal(t, 1, repo2.setCalls)
}

// 计数查询失败时放行熔断：保底的目的是防误摘全池，
// 不应因一次查询失败让熔断整体失效。
func TestUpstream5xxBreakerFailsOpenWhenCountUnavailable(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.MinSamples = 10
	settings.MinHealthyAccounts = 5
	repo := &upstream5xxAccountRepo{schedulableErr: context.DeadlineExceeded}
	svc := newUpstream5xxService(t, settings, repo)
	account := upstream5xxTestAccount()
	ctx := context.Background()

	tripped := false
	for i := 0; i < 20; i++ {
		if svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.5") {
			tripped = true
			break
		}
	}
	require.True(t, tripped)
}

// 反复熔断必须指数退避，否则坏账号每个冷却周期都回来伤一次。
func TestUpstream5xxBreakerExponentialBackoff(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.CooldownSeconds = 60
	settings.MaxCooldownSeconds = 480
	svc := newUpstream5xxService(t, settings, &upstream5xxAccountRepo{schedulableCount: 100})
	now := time.Now()

	got := []time.Duration{}
	for i := 0; i < 5; i++ {
		got = append(got, svc.nextUpstream5xxCooldown(99, settings, now))
	}

	require.Equal(t, []time.Duration{
		60 * time.Second,
		120 * time.Second,
		240 * time.Second,
		480 * time.Second,
		480 * time.Second, // 封顶
	}, got)
}

// MaxCooldownSeconds <= CooldownSeconds 表示关闭退避，恒定回避时长。
func TestUpstream5xxBreakerBackoffDisabled(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.CooldownSeconds = 90
	settings.MaxCooldownSeconds = 0
	svc := newUpstream5xxService(t, settings, &upstream5xxAccountRepo{schedulableCount: 100})
	now := time.Now()

	for i := 0; i < 4; i++ {
		require.Equal(t, 90*time.Second, svc.nextUpstream5xxCooldown(101, settings, now))
	}
}

// 账号被证明健康后清除退避档位，下次熔断回到初始档位。
func TestUpstream5xxBreakerResetsStreakAfterHealthyWindow(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.MinSamples = 10
	settings.ErrorRatePercent = 40
	settings.CooldownSeconds = 60
	settings.MaxCooldownSeconds = 480
	svc := newUpstream5xxService(t, settings, &upstream5xxAccountRepo{schedulableCount: 100})
	account := upstream5xxTestAccount()
	ctx := context.Background()
	now := time.Now()

	// 推进两档退避。
	require.Equal(t, 60*time.Second, svc.nextUpstream5xxCooldown(account.ID, settings, now))
	require.Equal(t, 120*time.Second, svc.nextUpstream5xxCooldown(account.ID, settings, now))

	// 窗口内样本充足且错误率回落 → 档位清零。
	for i := 0; i < 20; i++ {
		svc.observeUpstream5xxSuccess(ctx, account)
	}
	require.Equal(t, 60*time.Second, svc.nextUpstream5xxCooldown(account.ID, settings, now),
		"proven-healthy account must fall back to the base cooldown")
}

// 窗口过期后计数归零，旧窗口的失败不能污染新窗口。
func TestUpstream5xxWindowExpiryResetsCounts(t *testing.T) {
	svc := &OpenAIGatewayService{}
	window := 60 * time.Second
	base := time.Now()

	for i := 0; i < 5; i++ {
		svc.observeUpstream5xxOutcome(1, true, window, base)
	}
	failures, total := svc.observeUpstream5xxOutcome(1, true, window, base)
	require.Equal(t, 6, failures)
	require.Equal(t, 6, total)

	// 跨过窗口边界。
	failures, total = svc.observeUpstream5xxOutcome(1, true, window, base.Add(90*time.Second))
	require.Equal(t, 1, failures)
	require.Equal(t, 1, total)
}

// 时钟回拨也要重置，避免 now.Sub(start) 为负导致窗口永不过期。
func TestUpstream5xxWindowHandlesClockSkew(t *testing.T) {
	svc := &OpenAIGatewayService{}
	window := 60 * time.Second
	base := time.Now()

	svc.observeUpstream5xxOutcome(2, true, window, base)
	failures, total := svc.observeUpstream5xxOutcome(2, true, window, base.Add(-30*time.Second))
	require.Equal(t, 1, failures)
	require.Equal(t, 1, total)
}

// 熔断后窗口清零，避免残留计数在回避结束后立即再次触发。
func TestUpstream5xxBreakerResetsWindowAfterTrip(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.MinSamples = 10
	repo := &upstream5xxAccountRepo{schedulableCount: 100}
	svc := newUpstream5xxService(t, settings, repo)
	account := upstream5xxTestAccount()
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		if svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.5") {
			break
		}
	}
	require.Equal(t, 1, repo.setCalls)

	// 熔断后单次失败不应立刻再次触发（窗口已清零，样本不足）。
	require.False(t, svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.5"))
	require.Equal(t, 1, repo.setCalls)
}

// Spark 影子账号不参与账号级熔断，与 429 路径保持一致。
func TestUpstream5xxBreakerSkipsShadowAccount(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.MinSamples = 1
	repo := &upstream5xxAccountRepo{schedulableCount: 100}
	svc := newUpstream5xxService(t, settings, repo)
	ctx := context.Background()

	parentID := int64(5)
	account := upstream5xxTestAccount()
	account.ParentAccountID = &parentID
	require.True(t, account.IsShadow())

	for i := 0; i < 20; i++ {
		require.False(t, svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.5"))
	}
	require.Zero(t, repo.setCalls)
}

func TestNormalizeUpstream5xxBreakerFieldsBackfillsEmptyConfig(t *testing.T) {
	// 空配置（老数据）整体回填默认值，但保留 Enabled。
	settings := &Upstream5xxBreakerSettings{Enabled: true}
	normalizeUpstream5xxBreakerFields(settings)
	def := DefaultUpstream5xxBreakerSettings()

	require.True(t, settings.Enabled)
	require.Equal(t, def.WindowSeconds, settings.WindowSeconds)
	require.Equal(t, def.MinSamples, settings.MinSamples)
	require.Equal(t, def.ErrorRatePercent, settings.ErrorRatePercent)
	require.Equal(t, def.CooldownSeconds, settings.CooldownSeconds)
}

func TestNormalizeUpstream5xxBreakerFieldsClampsBounds(t *testing.T) {
	settings := &Upstream5xxBreakerSettings{
		Enabled:            true,
		WindowSeconds:      5,
		MinSamples:         0,
		ErrorRatePercent:   0,
		CooldownSeconds:    -1,
		MaxCooldownSeconds: -5,
		MinHealthyAccounts: -3,
	}
	normalizeUpstream5xxBreakerFields(settings)
	def := DefaultUpstream5xxBreakerSettings()

	require.Equal(t, 30, settings.WindowSeconds, "window floor")
	require.Equal(t, def.MinSamples, settings.MinSamples)
	// 错误率为 0 会让任何账号立即熔断，必须回落到默认值。
	require.Equal(t, def.ErrorRatePercent, settings.ErrorRatePercent)
	require.Equal(t, def.CooldownSeconds, settings.CooldownSeconds)
	require.Zero(t, settings.MaxCooldownSeconds)
	require.Zero(t, settings.MinHealthyAccounts)
}

func TestUpstream5xxBreakerReasonFormat(t *testing.T) {
	reason := upstream5xxBreakerReason(68, 300, 34, 50)
	require.Equal(t, "upstream 5xx breaker: rate=68% window=300s failures=34/50", reason)
	// 前缀必须与已有的 "token refresh retry exhausted:" 匹配逻辑区分开。
	require.NotContains(t, reason, "token refresh retry exhausted")
}

func TestShouldCooldownOpenAITransientUpstreamErrorCovers5xx(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504, 520, 521, 522, 523, 524} {
		require.True(t, shouldCooldownOpenAITransientUpstreamError(code, nil), "status %d", code)
	}
	for _, code := range []int{200, 401, 403, 404, 429} {
		require.False(t, shouldCooldownOpenAITransientUpstreamError(code, nil), "status %d", code)
	}
}
