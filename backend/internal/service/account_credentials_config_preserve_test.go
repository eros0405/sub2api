//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 重新授权的真实形状：前端 buildCredentials 只产出 token 类字段，
// 管理员配置的 model_mapping 等必须存活下来。
func TestPreserveAdminConfigCreds_ReauthKeepsModelMapping(t *testing.T) {
	existing := map[string]any{
		"access_token":          "at-old",
		"refresh_token":         "rt-old",
		"model_mapping":         map[string]any{"gpt-5.6-sol": "gpt-5.6"},
		"compact_model_mapping": map[string]any{"gpt-5.6-sol": "gpt-5.6-compact"},
		"pool_mode":             "same_account",
	}
	incoming := map[string]any{
		"access_token":  "at-new",
		"refresh_token": "rt-new",
		"expires_at":    "2026-09-18T00:00:00Z",
	}

	out := PreserveAdminConfigCreds(existing, incoming)

	require.Equal(t, map[string]any{"gpt-5.6-sol": "gpt-5.6"}, out["model_mapping"],
		"重新授权不得清空账号模型限制")
	require.Equal(t, map[string]any{"gpt-5.6-sol": "gpt-5.6-compact"}, out["compact_model_mapping"])
	require.Equal(t, "same_account", out["pool_mode"])
	require.Equal(t, "at-new", out["access_token"], "token 仍以新授权为准")
	require.Equal(t, "rt-new", out["refresh_token"])
	require.Equal(t, "2026-09-18T00:00:00Z", out["expires_at"])
}

// incoming 显式提供配置键时以 incoming 为准，包括显式清空。
func TestPreserveAdminConfigCreds_IncomingWins(t *testing.T) {
	existing := map[string]any{
		"model_mapping": map[string]any{"old": "mapping"},
		"base_url":      "https://old.example.com",
	}
	incoming := map[string]any{
		"model_mapping": map[string]any{"new": "mapping"},
		"base_url":      nil,
	}

	out := PreserveAdminConfigCreds(existing, incoming)

	require.Equal(t, map[string]any{"new": "mapping"}, out["model_mapping"])
	require.Contains(t, out, "base_url", "显式传 nil 属于「已提供」，不应被 existing 覆盖")
	require.Nil(t, out["base_url"])
}

func TestPreserveAdminConfigCreds_NilIncomingStaysNil(t *testing.T) {
	existing := map[string]any{"model_mapping": map[string]any{"foo": "bar"}}
	require.Nil(t, PreserveAdminConfigCreds(existing, nil),
		"nil incoming 表示「未提供 credentials」，语义须原样传递给下游")
}

func TestPreserveAdminConfigCreds_DoesNotMutateInputs(t *testing.T) {
	existing := map[string]any{"model_mapping": map[string]any{"foo": "bar"}}
	incoming := map[string]any{"access_token": "at-new"}

	_ = PreserveAdminConfigCreds(existing, incoming)

	require.Len(t, existing, 1)
	require.Len(t, incoming, 1, "incoming 不得被回填污染")
	require.NotContains(t, incoming, "model_mapping")
}

// 回填后的结果继续流经 MergePreservingSensitiveCreds（UpdateAccount 的真实下游），
// 配置键与敏感键都应完好。
func TestPreserveAdminConfigCreds_SurvivesDownstreamMerge(t *testing.T) {
	existing := map[string]any{
		"access_token":  "at-old",
		"api_key":       "sk-old",
		"model_mapping": map[string]any{"gpt-5.6-sol": "gpt-5.6"},
	}
	incoming := PreserveAdminConfigCreds(existing, map[string]any{"access_token": "at-new"})

	out := MergePreservingSensitiveCreds(existing, incoming)

	require.Equal(t, map[string]any{"gpt-5.6-sol": "gpt-5.6"}, out["model_mapping"])
	require.Equal(t, "at-new", out["access_token"])
	require.Equal(t, "sk-old", out["api_key"])
}
