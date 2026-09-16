package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func businessPremiumAccount() *Account {
	return &Account{
		ID:          78,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"plan_type": "self_serve_business_prolite"},
	}
}

func TestNormalizeOpenAIPlanTypeStripsSeparators(t *testing.T) {
	require.Equal(t, "selfservebusinessprolite", normalizeOpenAIPlanType("self_serve_business_prolite"))
	require.Equal(t, "selfservebusinessprolite", normalizeOpenAIPlanType("  Self-Serve Business ProLite "))
	require.Equal(t, "chatgptpro", normalizeOpenAIPlanType("chatgpt_pro"))
	require.Equal(t, "", normalizeOpenAIPlanType(""))
}

func TestIsOpenAIBusinessPremium(t *testing.T) {
	require.True(t, businessPremiumAccount().IsOpenAIBusinessPremium())

	// extra 回退路径：部分导入/探测路径只把 plan_type 落在 extra 上。
	fromExtra := &Account{
		ID:       79,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"plan_type": "self_serve_business_prolite"},
	}
	require.True(t, fromExtra.IsOpenAIBusinessPremium())

	// Business Standard（team）与 usage-based business 都不是 Business Premium。
	for _, planType := range []string{"team", "self_serve_business_usage_based", "pro", "plus", ""} {
		account := businessPremiumAccount()
		account.Credentials["plan_type"] = planType
		require.False(t, account.IsOpenAIBusinessPremium(), "plan_type=%q must not match", planType)
	}

	// 非 OpenAI 平台即使 plan_type 同名也不匹配。
	other := businessPremiumAccount()
	other.Platform = PlatformAnthropic
	require.False(t, other.IsOpenAIBusinessPremium())

	require.False(t, (*Account)(nil).IsOpenAIBusinessPremium())
}

// 未配置（老数据）时默认生效，保证升级前后行为一致。
func TestAppliesToBusinessPremiumDefaultsToTrue(t *testing.T) {
	require.True(t, (&RateLimit429CooldownSettings{}).AppliesToBusinessPremium())
	require.True(t, (*RateLimit429CooldownSettings)(nil).AppliesToBusinessPremium())
	require.True(t, (&Upstream5xxBreakerSettings{}).AppliesToBusinessPremium())
	require.True(t, (*Upstream5xxBreakerSettings)(nil).AppliesToBusinessPremium())

	require.False(t, (&RateLimit429CooldownSettings{ApplyToBusinessPremium: boolPtr(false)}).AppliesToBusinessPremium())
	require.False(t, (&Upstream5xxBreakerSettings{ApplyToBusinessPremium: boolPtr(false)}).AppliesToBusinessPremium())
}

func TestUpstream5xxBreakerSkipsBusinessPremiumWhenExcluded(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.ApplyToBusinessPremium = boolPtr(false)

	repo := &upstream5xxAccountRepo{schedulableCount: 100}
	svc := newUpstream5xxService(t, settings, repo)
	account := businessPremiumAccount()
	ctx := context.Background()

	for i := 0; i < 30; i++ {
		require.False(t, svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.6-sol"))
	}
	require.Zero(t, repo.setCalls, "excluded account must never be persisted as unschedulable")
}

// 开关打开（或未配置）时 Business Premium 与普通账号一样熔断。
func TestUpstream5xxBreakerTripsBusinessPremiumWhenIncluded(t *testing.T) {
	repo := &upstream5xxAccountRepo{schedulableCount: 100}
	svc := newUpstream5xxService(t, upstream5xxTestSettings(), repo)
	account := businessPremiumAccount()
	ctx := context.Background()

	tripped := false
	for i := 0; i < 30 && !tripped; i++ {
		tripped = svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.6-sol")
	}
	require.True(t, tripped)
	require.Equal(t, 1, repo.setCalls)
}

// 豁免账号不参与窗口计数：开关切回后不应残留一段陈旧样本立即熔断。
func TestUpstream5xxBreakerExcludedAccountDoesNotAccumulateSamples(t *testing.T) {
	settings := upstream5xxTestSettings()
	settings.ApplyToBusinessPremium = boolPtr(false)

	repo := &upstream5xxAccountRepo{schedulableCount: 100}
	svc := newUpstream5xxService(t, settings, repo)
	account := businessPremiumAccount()
	ctx := context.Background()

	for i := 0; i < 30; i++ {
		svc.maybeTripUpstream5xxBreaker(ctx, account, 503, "gpt-5.6-sol")
		svc.observeUpstream5xxSuccess(ctx, account)
	}
	_, ok := svc.openaiUpstream5xxWindows.Load(account.ID)
	require.False(t, ok, "excluded account must not create a sliding window")
}

func newRateLimit429Service(t *testing.T, settings *RateLimit429CooldownSettings) *SettingService {
	t.Helper()
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	return &SettingService{
		settingRepo: &fakeSettingRepo{vals: map[string]string{
			SettingKeyRateLimit429CooldownSettings: string(raw),
		}},
	}
}

func TestGet429FallbackCooldownSkipsBusinessPremiumWhenExcluded(t *testing.T) {
	settings := DefaultRateLimit429CooldownSettings()
	settings.ApplyToBusinessPremium = boolPtr(false)
	svc := &RateLimitService{settingService: newRateLimit429Service(t, settings)}
	ctx := context.Background()

	_, enabled := svc.get429FallbackCooldown(ctx, businessPremiumAccount())
	require.False(t, enabled, "excluded Business Premium account must not get the default cooldown")

	// 普通账号不受影响。
	plus := businessPremiumAccount()
	plus.Credentials["plan_type"] = "plus"
	cooldown, enabled := svc.get429FallbackCooldown(ctx, plus)
	require.True(t, enabled)
	require.Positive(t, cooldown)
}

func TestGet429FallbackCooldownAppliesToBusinessPremiumByDefault(t *testing.T) {
	svc := &RateLimitService{settingService: newRateLimit429Service(t, DefaultRateLimit429CooldownSettings())}

	cooldown, enabled := svc.get429FallbackCooldown(context.Background(), businessPremiumAccount())
	require.True(t, enabled)
	require.Positive(t, cooldown)
}

func TestShouldTrip429BreakerSkipsBusinessPremiumWhenExcluded(t *testing.T) {
	settings := DefaultRateLimit429CooldownSettings()
	settings.TransientThreshold = 1
	settings.ApplyToBusinessPremium = boolPtr(false)
	svc := &OpenAIGatewayService{settingService: newRateLimit429Service(t, settings)}
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.False(t, svc.shouldTrip429Breaker(ctx, businessPremiumAccount()))
	}
	require.False(t, svc.rateLimit429AppliesTo(ctx, businessPremiumAccount()))

	plus := businessPremiumAccount()
	plus.Credentials["plan_type"] = "plus"
	require.True(t, svc.shouldTrip429Breaker(ctx, plus))
	require.True(t, svc.rateLimit429AppliesTo(ctx, plus))
}

// settingService 读不到配置时保持默认回避行为，不因豁免逻辑意外放行。
func TestRateLimit429AppliesToDefaultsTrueWithoutSettings(t *testing.T) {
	svc := &OpenAIGatewayService{}
	require.True(t, svc.rateLimit429AppliesTo(context.Background(), businessPremiumAccount()))
}

// 阈值字段为 0 的老数据在归一化时不得覆盖显式设置的 ApplyToBusinessPremium。
func TestNormalizeUpstream5xxBreakerFieldsPreservesBusinessPremiumFlag(t *testing.T) {
	settings := &Upstream5xxBreakerSettings{Enabled: true, ApplyToBusinessPremium: boolPtr(false)}
	normalizeUpstream5xxBreakerFields(settings)

	require.True(t, settings.Enabled)
	require.False(t, settings.AppliesToBusinessPremium())
	require.Equal(t, DefaultUpstream5xxBreakerSettings().WindowSeconds, settings.WindowSeconds)
}
