package service

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// 粘性会话并发溢出（sticky overflow）
//
// 账号并发槽位的本意是保护上游，不是本地限流。但粘性会话撞上槽位满时，原有逻辑
// 只有两条路：进粘性排队队列等一会（牺牲延迟），或换号（牺牲 prompt cache 命中
// 与上下文连续性）。溢出配额给出第三条路：在 concurrency + N 的放宽上限下再抢一
// 次槽位，抢到就立刻转发。
//
// 关键约束：
//   - 溢出仍有硬顶（N ≤ StickySessionOverflowSlotsMax），不是无上限突破。上游压力
//     可控，与"粘性无条件跳过并发限制"不是一回事。
//   - 只对粘性命中的账号生效。非粘性选号仍走原始 concurrency，否则整个并发上限失效。
//   - account.Concurrency <= 0 表示该账号本就不限并发，此时溢出无意义且不能把
//     "不限"退化成 N（见 stickyOverflowConcurrency）。
const (
	// StickySessionOverflowSlotsMax 溢出槽位可配置的最大值。
	StickySessionOverflowSlotsMax = 5

	stickyOverflowSettingCacheTTL  = 30 * time.Second
	stickyOverflowSettingDBTimeout = 2 * time.Second
	stickyOverflowSettingCacheKey  = SettingKeyStickySessionOverflowSlots
)

type cachedStickyOverflowSetting struct {
	slots     int
	expiresAt int64
}

var (
	stickyOverflowSettingCache atomic.Value // *cachedStickyOverflowSetting
	stickyOverflowSettingSF    singleflight.Group
)

// ParseStickySessionOverflowSlots 归一化溢出槽位配置：非法/负值按 0（关闭）处理，
// 超过上限按上限截断。管理端写入时已做校验，这里是读侧的兜底，防止手工改库
// 或旧数据把放宽上限撑到不可控。
func ParseStickySessionOverflowSlots(raw string) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0
	}
	if value > StickySessionOverflowSlotsMax {
		return StickySessionOverflowSlotsMax
	}
	return value
}

// stickyOverflowConcurrency 返回粘性溢出时使用的放宽并发上限。
//
// 返回 0 表示"不要重试溢出"：既包括未开启溢出，也包括账号本身不限并发
// （baseConcurrency <= 0，此时首次抢槽必然成功，不会走到溢出分支）。
// 这里不能对 baseConcurrency <= 0 返回 slots——那会把"不限并发"收紧成 N。
func stickyOverflowConcurrency(baseConcurrency, slots int) int {
	if slots <= 0 || baseConcurrency <= 0 {
		return 0
	}
	if slots > StickySessionOverflowSlotsMax {
		slots = StickySessionOverflowSlotsMax
	}
	return baseConcurrency + slots
}

func stickyOverflowSlotsFromRepo(ctx context.Context, repo SettingRepository) int {
	if cached, ok := stickyOverflowSettingCache.Load().(*cachedStickyOverflowSetting); ok && cached != nil {
		if time.Now().UnixNano() < cached.expiresAt {
			return cached.slots
		}
	}
	if repo == nil {
		return 0
	}

	result, _, _ := stickyOverflowSettingSF.Do(stickyOverflowSettingCacheKey, func() (any, error) {
		if cached, ok := stickyOverflowSettingCache.Load().(*cachedStickyOverflowSetting); ok && cached != nil {
			if time.Now().UnixNano() < cached.expiresAt {
				return cached.slots, nil
			}
		}

		// 调度热路径：DB 读取必须有独立超时，且不能被请求 ctx 取消带走缓存写入。
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stickyOverflowSettingDBTimeout)
		defer cancel()

		slots := 0
		if value, err := repo.GetValue(dbCtx, SettingKeyStickySessionOverflowSlots); err == nil {
			slots = ParseStickySessionOverflowSlots(value)
		}
		// 读失败按 0 缓存一个 TTL：溢出是放宽保护的开关，读不到时宁可不放宽。
		storeStickyOverflowSettingCache(slots)
		return slots, nil
	})

	slots, _ := result.(int)
	return slots
}

func storeStickyOverflowSettingCache(slots int) {
	stickyOverflowSettingCache.Store(&cachedStickyOverflowSetting{
		slots:     slots,
		expiresAt: time.Now().Add(stickyOverflowSettingCacheTTL).UnixNano(),
	})
}

// clampStickySessionOverflowSlots 把写入值收敛到 [0, StickySessionOverflowSlotsMax]。
func clampStickySessionOverflowSlots(slots int) int {
	if slots <= 0 {
		return 0
	}
	if slots > StickySessionOverflowSlotsMax {
		return StickySessionOverflowSlotsMax
	}
	return slots
}

// WarmStickySessionOverflowSlotsCache 在设置保存后同步刷新缓存，避免管理员改完
// 还要等一个 TTL 才生效。
func WarmStickySessionOverflowSlotsCache(slots int) {
	stickyOverflowSettingSF.Forget(stickyOverflowSettingCacheKey)
	storeStickyOverflowSettingCache(clampStickySessionOverflowSlots(slots))
}

func resetStickyOverflowSettingCacheForTest() {
	stickyOverflowSettingCache = atomic.Value{}
	stickyOverflowSettingSF = singleflight.Group{}
}

// stickySessionOverflowSlots 读取 Claude 侧的溢出配额。
func (s *GatewayService) stickySessionOverflowSlots(ctx context.Context) int {
	if s == nil || s.settingService == nil {
		return 0
	}
	return stickyOverflowSlotsFromRepo(ctx, s.settingService.settingRepo)
}

// stickySessionOverflowSlots 读取 OpenAI 侧的溢出配额。
func (s *OpenAIGatewayService) stickySessionOverflowSlots(ctx context.Context) int {
	if s == nil || s.settingService == nil {
		return 0
	}
	return stickyOverflowSlotsFromRepo(ctx, s.settingService.settingRepo)
}
