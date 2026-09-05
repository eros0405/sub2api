//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// newBreakerGatewayService 构造带 settingService 的网关实例，并写入指定的 429 回避配置。
func newBreakerGatewayService(t *testing.T, settings RateLimit429CooldownSettings) *OpenAIGatewayService {
	t.Helper()
	repo := newMockSettingRepo()
	data, _ := json.Marshal(settings)
	repo.data[SettingKeyRateLimit429CooldownSettings] = string(data)
	settingSvc := NewSettingService(repo, &config.Config{})
	return &OpenAIGatewayService{settingService: settingSvc}
}

// oauthAccountForBreaker 返回一个处于重试窗口早期的 OAuth 账号，
// 使 markOpenAIOAuth429RateLimited 落入 Transient + 窗口活跃分支。
func oauthAccountForBreaker(svc *OpenAIGatewayService, id int64) *Account {
	svc.openaiOAuth429RetryStartedAt.Store(id, time.Now())
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
}

func TestTransient429Breaker_KeepsRetryWindowBelowThreshold(t *testing.T) {
	svc := newBreakerGatewayService(t, RateLimit429CooldownSettings{
		Enabled: true, CooldownSeconds: 5, WindowSeconds: 60, TransientThreshold: 5, MaxCooldownSeconds: 600,
	})
	account := oauthAccountForBreaker(svc, 1001)

	// 未达阈值：窗口活跃期间不应熔断（保持同账号重试，不 block）。
	for i := 0; i < 4; i++ {
		svc.markOpenAIOAuth429RateLimited(context.Background(), account, http.Header{}, nil)
		require.False(t, svc.isOpenAIAccountRuntimeBlocked(account), "第 %d 次瞬时429不应熔断", i+1)
	}
}

func TestTransient429Breaker_TripsAtThreshold(t *testing.T) {
	svc := newBreakerGatewayService(t, RateLimit429CooldownSettings{
		Enabled: true, CooldownSeconds: 5, WindowSeconds: 60, TransientThreshold: 5, MaxCooldownSeconds: 600,
	})
	account := oauthAccountForBreaker(svc, 1002)

	for i := 0; i < 5; i++ {
		svc.markOpenAIOAuth429RateLimited(context.Background(), account, http.Header{}, nil)
	}
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account), "达到阈值后应立即熔断进入回避")
}

func TestTransient429Breaker_ThresholdZeroKeepsLegacyBehavior(t *testing.T) {
	svc := newBreakerGatewayService(t, RateLimit429CooldownSettings{
		Enabled: true, CooldownSeconds: 5, WindowSeconds: 60, TransientThreshold: 0, MaxCooldownSeconds: 600,
	})
	account := oauthAccountForBreaker(svc, 1003)

	for i := 0; i < 20; i++ {
		svc.markOpenAIOAuth429RateLimited(context.Background(), account, http.Header{}, nil)
		require.False(t, svc.isOpenAIAccountRuntimeBlocked(account), "阈值=0应关闭熔断，始终保持旧行为")
	}
}

func TestTransient429Breaker_ExponentialBackoffAndReset(t *testing.T) {
	base := 5 * time.Second
	svc := newBreakerGatewayService(t, RateLimit429CooldownSettings{
		Enabled: true, CooldownSeconds: 5, WindowSeconds: 60, TransientThreshold: 1, MaxCooldownSeconds: 600,
	})
	account := &Account{ID: 1004, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	// streak=0 → base
	d0 := svc.next429TransientCooldown(context.Background(), account)
	require.Equal(t, base, d0)
	// streak=1 → base*2
	d1 := svc.next429TransientCooldown(context.Background(), account)
	require.Equal(t, base*2, d1)
	// streak=2 → base*4
	d2 := svc.next429TransientCooldown(context.Background(), account)
	require.Equal(t, base*4, d2)

	// 成功恢复后 streak 归零，回到 base。
	svc.reset429TransientState(account.ID)
	dReset := svc.next429TransientCooldown(context.Background(), account)
	require.Equal(t, base, dReset)
}

func TestTransient429Breaker_BackoffCappedAtMax(t *testing.T) {
	svc := newBreakerGatewayService(t, RateLimit429CooldownSettings{
		Enabled: true, CooldownSeconds: 100, WindowSeconds: 60, TransientThreshold: 1, MaxCooldownSeconds: 300,
	})
	account := &Account{ID: 1005, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	maxCooldown := 300 * time.Second
	var last time.Duration
	for i := 0; i < 10; i++ {
		last = svc.next429TransientCooldown(context.Background(), account)
		require.LessOrEqual(t, last, maxCooldown, "冷却不得超过封顶")
	}
	require.Equal(t, maxCooldown, last, "反复熔断后应收敛到封顶值")
}

func TestTransient429Breaker_NoBackoffWhenMaxNotGreaterThanBase(t *testing.T) {
	base := 5 * time.Second
	svc := newBreakerGatewayService(t, RateLimit429CooldownSettings{
		Enabled: true, CooldownSeconds: 5, WindowSeconds: 60, TransientThreshold: 1, MaxCooldownSeconds: 0,
	})
	account := &Account{ID: 1006, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	// max<=base 时不退避：每次都是 base。
	for i := 0; i < 5; i++ {
		require.Equal(t, base, svc.next429TransientCooldown(context.Background(), account))
	}
}
