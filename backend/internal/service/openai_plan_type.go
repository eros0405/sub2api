package service

import "strings"

// ChatGPT 订阅档位（plan_type）的归一化与识别。
//
// plan_type 由上游原样透传，同一档位会出现 `chatgpt_pro`、`self_serve_business_prolite`
// 这类下划线/别名写法，因此匹配前一律先归一化。与前端 utils/planType.ts 的
// normalizePlanType 保持同一套规则，避免两侧对同一档位判断不一致。

// openAIBusinessPremiumPlanType 是 Business Premium 归一化后的 plan_type。
// 上游原始值为 `self_serve_business_prolite`。
const openAIBusinessPremiumPlanType = "selfservebusinessprolite"

// normalizeOpenAIPlanType 去首尾空白、转小写，并去掉空格/下划线/连字符。
// 例：`self_serve_business_prolite` → `selfservebusinessprolite`。
func normalizeOpenAIPlanType(value string) string {
	lowered := strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	b.Grow(len(lowered))
	for _, r := range lowered {
		switch r {
		case ' ', '\t', '\n', '\r', '_', '-':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// IsOpenAIBusinessPremium 判断账号是否为 ChatGPT Business Premium 档位。
//
// plan_type 优先取 credentials，回退 extra：OAuth 刷新写入 credentials，
// 而部分导入/探测路径只落在 extra 上（与 grok_free_quota_gate 的读取顺序一致）。
func (a *Account) IsOpenAIBusinessPremium() bool {
	if a == nil || !a.IsOpenAI() {
		return false
	}
	for _, candidate := range []string{
		a.GetCredential("plan_type"),
		a.GetExtraString("plan_type"),
	} {
		if normalizeOpenAIPlanType(candidate) == openAIBusinessPremiumPlanType {
			return true
		}
	}
	return false
}
