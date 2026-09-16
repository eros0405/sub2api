package service

import "testing"

func TestParseStickySessionOverflowSlots(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{name: "empty disables", raw: "", want: 0},
		{name: "zero disables", raw: "0", want: 0},
		{name: "negative disables", raw: "-3", want: 0},
		{name: "garbage disables", raw: "abc", want: 0},
		{name: "in range", raw: "2", want: 2},
		{name: "trimmed", raw: "  4  ", want: 4},
		{name: "at max", raw: "5", want: 5},
		// 手工改库或旧数据可能塞进超界值，读侧必须截断而不是原样放大上限。
		{name: "above max truncated", raw: "50", want: StickySessionOverflowSlotsMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseStickySessionOverflowSlots(tc.raw); got != tc.want {
				t.Fatalf("ParseStickySessionOverflowSlots(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestStickyOverflowConcurrency(t *testing.T) {
	cases := []struct {
		name  string
		base  int
		slots int
		want  int
	}{
		{name: "disabled", base: 4, slots: 0, want: 0},
		{name: "negative slots", base: 4, slots: -1, want: 0},
		{name: "relaxes limit", base: 4, slots: 2, want: 6},
		{name: "slots at max", base: 10, slots: 5, want: 15},
		// base <= 0 表示账号不限并发：不能把"不限"退化成 slots，否则开启溢出
		// 反而给无限并发的账号加上了一个上限。
		{name: "unlimited account untouched", base: 0, slots: 3, want: 0},
		{name: "negative base untouched", base: -1, slots: 3, want: 0},
		// 防御性截断：即便调用方传进超界 slots 也不放大到硬顶之上。
		{name: "slots above max clamped", base: 4, slots: 99, want: 4 + StickySessionOverflowSlotsMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stickyOverflowConcurrency(tc.base, tc.slots); got != tc.want {
				t.Fatalf("stickyOverflowConcurrency(%d, %d) = %d, want %d", tc.base, tc.slots, got, tc.want)
			}
		})
	}
}

func TestClampStickySessionOverflowSlots(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{in: -5, want: 0},
		{in: 0, want: 0},
		{in: 3, want: 3},
		{in: StickySessionOverflowSlotsMax, want: StickySessionOverflowSlotsMax},
		{in: 100, want: StickySessionOverflowSlotsMax},
	}
	for _, tc := range cases {
		if got := clampStickySessionOverflowSlots(tc.in); got != tc.want {
			t.Fatalf("clampStickySessionOverflowSlots(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestWarmStickySessionOverflowSlotsCacheServesWithoutRepo(t *testing.T) {
	t.Cleanup(resetStickyOverflowSettingCacheForTest)
	resetStickyOverflowSettingCacheForTest()

	// nil repo 且无缓存：必须按关闭处理，不能默认放宽上限。
	if got := stickyOverflowSlotsFromRepo(t.Context(), nil); got != 0 {
		t.Fatalf("stickyOverflowSlotsFromRepo(nil repo) = %d, want 0", got)
	}

	// 保存设置后热缓存生效，管理员不用等一个 TTL。
	WarmStickySessionOverflowSlotsCache(3)
	if got := stickyOverflowSlotsFromRepo(t.Context(), nil); got != 3 {
		t.Fatalf("after warm = %d, want 3", got)
	}

	// 越界写入按硬顶收敛。
	WarmStickySessionOverflowSlotsCache(99)
	if got := stickyOverflowSlotsFromRepo(t.Context(), nil); got != StickySessionOverflowSlotsMax {
		t.Fatalf("after warm(99) = %d, want %d", got, StickySessionOverflowSlotsMax)
	}
}
