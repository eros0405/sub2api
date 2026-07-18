package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type openAIOAuthInputTokensAccountRepo struct {
	service.AccountRepository
	account              service.Account
	tempUnschedulableSet int
	errorSet             int
}

func (r *openAIOAuthInputTokensAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	if r.account.Platform != platform {
		return nil, nil
	}
	return []service.Account{r.account}, nil
}

func (r *openAIOAuthInputTokensAccountRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, _ int64, platform string) ([]service.Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}

func (r *openAIOAuthInputTokensAccountRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}

func (r *openAIOAuthInputTokensAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	if r.account.ID != id {
		return nil, nil
	}
	account := r.account
	return &account, nil
}

func (r *openAIOAuthInputTokensAccountRepo) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, _ string) error {
	r.tempUnschedulableSet++
	return nil
}

func (r *openAIOAuthInputTokensAccountRepo) SetError(_ context.Context, _ int64, _ string) error {
	r.errorSet++
	return nil
}

type openAIOAuthInputTokensUpstream struct {
	service.HTTPUpstream
	calls        int
	statusCode   int
	responseBody string
	lastPath     string
}

func (u *openAIOAuthInputTokensUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	u.lastPath = req.URL.Path
	statusCode := u.statusCode
	if statusCode == 0 {
		statusCode = http.StatusForbidden
	}
	responseBody := u.responseBody
	if responseBody == "" {
		responseBody = "<html>forbidden</html>"
	}
	return &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}, nil
}

func TestIsOpenAIResponsesInputTokensPath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		path string
		want bool
	}{
		{path: "/v1/responses/input_tokens", want: true},
		{path: "/responses/input_tokens/", want: true},
		{path: "/backend-api/codex/responses/input_tokens", want: true},
		{path: "/v1/responses", want: false},
		{path: "/v1/responses/compact", want: false},
		{path: "/v1/responses/other", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, nil)
			require.Equal(t, tt.want, isOpenAIResponsesInputTokensPath(c))
		})
	}
	require.False(t, isOpenAIResponsesInputTokensPath(nil))
}

func TestOpenAIResponses_InputTokensOAuthUsesLocalEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	paths := []string{
		"/v1/responses/input_tokens",
		"/responses/input_tokens",
		"/backend-api/codex/responses/input_tokens",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			groupID := int64(4491)
			accountRepo := &openAIOAuthInputTokensAccountRepo{account: service.Account{
				ID:          4491,
				Name:        "openai-oauth",
				Platform:    service.PlatformOpenAI,
				Type:        service.AccountTypeOAuth,
				Concurrency: 1,
				Credentials: map[string]any{
					"access_token":  "oauth-token",
					"refresh_token": "oauth-refresh-token",
				},
				Status:      service.StatusActive,
				Schedulable: true,
			}}
			upstream := &openAIOAuthInputTokensUpstream{}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Default.RateMultiplier = 1
			cfg.Security.URLAllowlist.Enabled = false

			billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billingCache.Stop)
			rateLimit := service.NewRateLimitService(accountRepo, nil, cfg, nil, nil)
			gateway := service.NewOpenAIGatewayService(
				accountRepo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
				service.NewBillingService(cfg, nil), rateLimit, billingCache, upstream,
				&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
			)
			h := NewOpenAIGatewayHandler(
				gateway,
				service.NewConcurrencyService(nil),
				billingCache,
				service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
				nil, nil, nil, nil, cfg,
			)

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(
				`{"model":"gpt-5.4","instructions":"Be concise.","input":"hello","stream":false}`,
			))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
				ID:      1,
				GroupID: &groupID,
				User:    &service.User{ID: 1, Status: service.StatusActive},
				Group:   &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
			})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1, Concurrency: 0})

			h.Responses(c)

			require.Equal(t, http.StatusOK, rec.Code)
			require.Positive(t, gjson.GetBytes(rec.Body.Bytes(), "input_tokens").Int())
			require.Zero(t, upstream.calls)
			require.Zero(t, accountRepo.tempUnschedulableSet)
			require.Zero(t, accountRepo.errorSet)
		})
	}
}

func TestOpenAIResponses_InputTokensAPIKeyKeepsUpstreamForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(4492)
	accountRepo := &openAIOAuthInputTokensAccountRepo{account: service.Account{
		ID:          4492,
		Name:        "openai-api-key",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.example.test",
		},
		Extra: map[string]any{
			"openai_passthrough":         true,
			"openai_responses_supported": true,
		},
		Status:      service.StatusActive,
		Schedulable: true,
	}}
	upstream := &openAIOAuthInputTokensUpstream{
		statusCode:   http.StatusOK,
		responseBody: `{"object":"response.input_tokens","input_tokens":42}`,
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false

	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	rateLimit := service.NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	gateway := service.NewOpenAIGatewayService(
		accountRepo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), rateLimit, billingCache, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
	h := NewOpenAIGatewayHandler(
		gateway,
		service.NewConcurrencyService(nil),
		billingCache,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil, cfg,
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", strings.NewReader(
		`{"model":"gpt-5.4","input":"hello","stream":false}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      2,
		GroupID: &groupID,
		User:    &service.User{ID: 2, Status: service.StatusActive},
		Group:   &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 0})

	h.Responses(c)

	require.Equal(t, http.StatusOK, rec.Code)
	require.EqualValues(t, 42, gjson.GetBytes(rec.Body.Bytes(), "input_tokens").Int())
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, "/v1/responses/input_tokens", upstream.lastPath)
}
