package service

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestFetchOpenAIAccountModelsOAuthPopulatesPickerFields(t *testing.T) {
	_, calls := newCodexModelsOAuthCacheServer(t, `{"models":[
		{"slug":"new-oauth-model","display_name":"New OAuth Model"},
		{"slug":"gpt-5.6-sol"},
		{"slug":"blank-display-name","display_name":"   "},
		{"slug":"gpt-6-astra"}
	]}`)
	gateway := &OpenAIGatewayService{}
	svc := &AccountTestService{openaiGatewayService: gateway}
	account := newCodexModelsTestAccount()
	ctx := context.Background()

	before, err := gateway.FetchOpenAIModelsList(ctx, account)
	require.NoError(t, err)
	models, err := svc.FetchOpenAIAccountModels(ctx, account)
	require.NoError(t, err)
	require.Greater(t, len(models), 2)
	// Upstream display name wins; a missing one falls back to the local catalog name,
	// then to the raw slug.
	for i, expected := range []struct{ id, displayName string }{
		{id: "new-oauth-model", displayName: "New OAuth Model"},
		{id: "gpt-5.6-sol", displayName: "GPT-5.6 Sol"},
		{id: "blank-display-name", displayName: "blank-display-name"},
		{id: "gpt-6-astra", displayName: "GPT-6 Astra"},
	} {
		require.Equal(t, expected.id, models[i].ID)
		require.Equal(t, expected.displayName, models[i].DisplayName)
		require.Equal(t, "model", models[i].Type)
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	require.Contains(t, ids, "gpt-image-2.5-flare")
	require.Contains(t, ids, "gpt-image-2.5-sunburst")

	after, err := gateway.FetchOpenAIModelsList(ctx, account)
	require.NoError(t, err)
	require.Equal(t, before.Body, after.Body, "picker fields must not change the shared catalog")
	require.Contains(t, string(after.Body), `"display_name":"New OAuth Model"`, "the shared catalog keeps the upstream display name")
	require.EqualValues(t, 1, calls.Load(), "picker must reuse the shared discovery cache")
}

func TestFetchOpenAIAccountModelsAPIKeyPopulatesPickerFields(t *testing.T) {
	gateway := newCodexModelsAPIKeyTestService(&codexModelsHTTPUpstreamStub{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		return ordinaryModelsUpstreamResponse(`{"data":[
			{"id":"new-api-model","owned_by":"provider","created":123},
			{"id":"blank-label","display_name":"  ","type":""},
			{"id":"named-model","display_name":"Provider Model","type":"model"}
		]}`), nil
	}})
	svc := &AccountTestService{openaiGatewayService: gateway}
	models, err := svc.FetchOpenAIAccountModels(context.Background(), newCodexModelsAPIKeyTestAccount("https://models.example/v1"))
	require.NoError(t, err)
	require.Len(t, models, 3)
	for i, name := range []string{"new-api-model", "blank-label", "Provider Model"} {
		require.Equal(t, name, models[i].DisplayName)
		require.Equal(t, "model", models[i].Type)
	}
	require.Equal(t, "provider", models[0].OwnedBy)
	require.EqualValues(t, 123, models[0].Created)
	require.Equal(t, "named-model", models[2].ID)
}

func TestFetchOpenAIAccountModelsAPIKeyIgnoresAccountModelMapping(t *testing.T) {
	gateway := newCodexModelsAPIKeyTestService(&codexModelsHTTPUpstreamStub{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		return ordinaryModelsUpstreamResponse(`{"data":[
			{"id":"upstream-target","owned_by":"provider","created":123},
			{"id":"direct-model","owned_by":"provider"},
			{"id":"unconfigured-model","owned_by":"provider"}
		]}`), nil
	}})
	svc := &AccountTestService{openaiGatewayService: gateway}
	account := newCodexModelsAPIKeyTestAccount("https://models.example/v1")
	account.Credentials["model_mapping"] = map[string]any{
		"configured-alias": "upstream-target",
		"direct-model":     "direct-model",
	}

	models, err := svc.FetchOpenAIAccountModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"upstream-target", "direct-model", "unconfigured-model"}, pickerModelIDs(models))
	require.Equal(t, "provider", models[0].OwnedBy)
	require.EqualValues(t, 123, models[0].Created)
}

func TestFetchOpenAIAccountModelsPreservesEmptyCatalog(t *testing.T) {
	gateway := newCodexModelsAPIKeyTestService(&codexModelsHTTPUpstreamStub{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		return ordinaryModelsUpstreamResponse(`{"data":[]}`), nil
	}})
	svc := &AccountTestService{openaiGatewayService: gateway}
	models, err := svc.FetchOpenAIAccountModels(context.Background(), newCodexModelsAPIKeyTestAccount("https://models.example/v1"))
	require.NoError(t, err)
	require.Empty(t, models, "an empty upstream catalog must not become a static model list")
}

func TestFetchOpenAIAccountModelsOAuthLabelsLocalImageModelsLikeUpstream(t *testing.T) {
	newCodexModelsOAuthCacheServer(t, `{"models":[{"slug":"gpt-5.6-sol"}]}`)
	svc := &AccountTestService{openaiGatewayService: &OpenAIGatewayService{}}
	account := newCodexModelsTestAccount()
	account.Credentials["model_mapping"] = map[string]any{
		"alias": "gpt-5.6-sol",
	}
	models, err := svc.FetchOpenAIAccountModels(context.Background(), account)
	require.NoError(t, err)
	byID := make(map[string]string, len(models))
	for _, model := range models {
		byID[model.ID] = model.DisplayName
	}
	require.Equal(t, "GPT-5.6 Sol", byID["gpt-5.6-sol"], "upstream slug must not be the only label source")
	require.Equal(t, "GPT Image 2.5 Flare", byID["gpt-image-2.5-flare"], "locally added models use the same naming rule")
}

func TestFetchOpenAIAccountModelsOAuthShowsAllNativeImageModels(t *testing.T) {
	newCodexModelsOAuthCacheServer(t, `{"models":[{"slug":"gpt-6-astra"}]}`)
	svc := &AccountTestService{openaiGatewayService: &OpenAIGatewayService{}}
	account := newCodexModelsTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"gpt-image-2.5-flare": "gpt-image-2.5-flare"}
	models, err := svc.FetchOpenAIAccountModels(context.Background(), account)
	require.NoError(t, err)
	ids := []string{}
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	require.Contains(t, ids, "gpt-image-2.5-flare")
	require.Contains(t, ids, "gpt-image-2.5-sunburst")
}

func pickerModelIDs(models []openai.Model) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func TestFetchOpenAIAccountModelsOAuthIncludesUnconfiguredTextModels(t *testing.T) {
	newCodexModelsOAuthCacheServer(t, `{"models":[{"slug":"gpt-5.6-sol"},{"slug":"gpt-6-astra"}]}`)
	svc := &AccountTestService{openaiGatewayService: &OpenAIGatewayService{}}
	account := newCodexModelsTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"gpt-5.6-sol": "gpt-5.6-sol"}

	models, err := svc.FetchOpenAIAccountModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.6-sol", "gpt-6-astra"}, pickerModelIDs(models)[:2])
}

func TestFetchOpenAIAccountModelsOAuthDoesNotAddImageAliases(t *testing.T) {
	newCodexModelsOAuthCacheServer(t, `{"models":[{"slug":"gpt-6-astra"}]}`)
	svc := &AccountTestService{openaiGatewayService: &OpenAIGatewayService{}}
	account := newCodexModelsTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"paint": "gpt-image-2.5-flare"}

	models, err := svc.FetchOpenAIAccountModels(context.Background(), account)
	require.NoError(t, err)
	require.Contains(t, pickerModelIDs(models), "gpt-6-astra")
	require.Contains(t, pickerModelIDs(models), "gpt-image-2.5-flare")
	require.NotContains(t, pickerModelIDs(models), "paint")
}

func TestFetchOpenAIAccountModelsOAuthPassthroughIgnoresMappingTargets(t *testing.T) {
	for _, flag := range []string{"openai_passthrough", "openai_oauth_passthrough"} {
		t.Run(flag, func(t *testing.T) {
			newCodexModelsOAuthCacheServer(t, `{"models":[{"slug":"gpt-6-astra"}]}`)
			svc := &AccountTestService{openaiGatewayService: &OpenAIGatewayService{}}
			account := newCodexModelsTestAccount()
			account.Extra = map[string]any{flag: true}
			account.Credentials["model_mapping"] = map[string]any{
				"gpt-image-custom":    "text-target-missing",
				"gpt-image-2.5-flare": "text-target-missing",
				"paint":               "gpt-image-2.5-flare",
			}

			models, err := svc.FetchOpenAIAccountModels(context.Background(), account)
			require.NoError(t, err)
			ids := pickerModelIDs(models)
			require.Contains(t, ids, "gpt-6-astra", "passthrough preserves the upstream catalog")
			require.Contains(t, ids, "gpt-image-2.5-flare")
			require.Contains(t, ids, "gpt-image-2.5-sunburst")
			require.NotContains(t, ids, "gpt-image-custom", "mapping aliases are not discovered models")
			require.NotContains(t, ids, "paint", "mapping aliases are not discovered models")
		})
	}
}

func TestFetchOpenAIAccountModelsAPIKeyPassthroughIgnoresMapping(t *testing.T) {
	gateway := newCodexModelsAPIKeyTestService(&codexModelsHTTPUpstreamStub{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		return ordinaryModelsUpstreamResponse(`{"data":[{"id":"target"},{"id":"other"}]}`), nil
	}})
	svc := &AccountTestService{openaiGatewayService: gateway}
	account := newCodexModelsAPIKeyTestAccount("https://models.example/v1")
	account.Extra = map[string]any{"openai_passthrough": true}
	account.Credentials["model_mapping"] = map[string]any{"paint": "gpt-image-2.5-flare"}

	models, err := svc.FetchOpenAIAccountModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"target", "other"}, pickerModelIDs(models))
}

func TestFetchOpenAIAccountModelsMappingChangesKeepRawCatalog(t *testing.T) {
	var calls atomic.Int32
	gateway := newCodexModelsAPIKeyTestService(&codexModelsHTTPUpstreamStub{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		calls.Add(1)
		return ordinaryModelsUpstreamResponse(`{"data":[
			{"id":"target","owned_by":"provider","created":123},
			{"id":"gpt-a"},
			{"id":"gpt-special"},
			{"id":"other","owned_by":"other-provider","created":456}
		]}`), nil
	}})
	svc := &AccountTestService{openaiGatewayService: gateway}
	account := newCodexModelsAPIKeyTestAccount("https://models.example/v1")
	account.Credentials["model_mapping"] = map[string]any{"gpt-*": "target", "gpt-special": "other"}
	ctx := context.Background()

	before, err := gateway.FetchOpenAIModelsList(ctx, account)
	require.NoError(t, err)
	original := append([]byte(nil), before.Body...)
	models, err := svc.FetchOpenAIAccountModels(ctx, account)
	require.NoError(t, err)
	expected := []string{"target", "gpt-a", "gpt-special", "other"}
	require.Equal(t, expected, pickerModelIDs(models))
	require.EqualValues(t, 123, models[0].Created)
	require.Equal(t, "provider", models[0].OwnedBy)
	require.EqualValues(t, 456, models[3].Created)
	require.Equal(t, "other-provider", models[3].OwnedBy)

	account.Credentials["model_mapping"] = map[string]any{"second": "other"}
	models, err = svc.FetchOpenAIAccountModels(ctx, account)
	require.NoError(t, err)
	require.Equal(t, expected, pickerModelIDs(models), "mapping changes must not alter the test catalog")
	after, err := gateway.FetchOpenAIModelsList(ctx, account)
	require.NoError(t, err)
	require.Equal(t, original, after.Body, "picker fields must not mutate the shared catalog")
	require.EqualValues(t, 1, calls.Load(), "mapping changes must reuse the same discovery cache")
}
