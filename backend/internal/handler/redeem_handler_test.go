package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type historyRepo struct {
	service.RedeemCodeRepository
	userID   int64
	params   pagination.PaginationParams
	codeType string
}

func (r *historyRepo) ListByUserPaginated(_ context.Context, userID int64, params pagination.PaginationParams, codeType string) ([]service.RedeemCode, *pagination.PaginationResult, error) {
	r.userID, r.params, r.codeType = userID, params, codeType
	// Mirror the real repository, which echoes the requested page/page_size back.
	return []service.RedeemCode{}, &pagination.PaginationResult{Total: 101, Page: params.Page, PageSize: params.PageSize}, nil
}

func TestRedeemHistory(t *testing.T) {
	for _, tt := range []struct {
		name, query, codeType string
		userID                int64
		status, page, size    int
	}{
		{"no params", "", "", 7, 200, 1, 20},
		{"default", "?page=1", "", 7, 200, 1, 20},
		{"size only", "?page_size=50", "", 7, 200, 1, 50},
		{"second user", "?page=2&page_size=100&user_id=7", "", 8, 200, 2, 100},
		{"size above max falls back", "?page_size=1001", "", 7, 200, 1, 20},
		{"beyond last", "?page=100&page_size=20", "", 7, 200, 100, 20},
		{"zero falls back", "?page=0", "", 7, 200, 1, 20},
		{"negative falls back", "?page_size=-1", "", 7, 200, 1, 20},
		{"invalid falls back", "?page=abc", "", 7, 200, 1, 20},
		{"type filter", "?type=balance", "balance", 7, 200, 1, 20},
		{"unauthenticated", "?page=1", "", 0, 401, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &historyRepo{}
			h := NewRedeemHandler(service.NewRedeemService(repo, nil, nil, nil, nil, nil, nil, nil))
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/api/v1/redeem/history"+tt.query, nil)
			if tt.userID != 0 {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: tt.userID})
			}
			h.GetHistory(c)
			require.Equal(t, tt.status, w.Code)
			if tt.status != 200 {
				require.Zero(t, repo.userID)
				return
			}
			require.Equal(t, tt.userID, repo.userID)
			require.Equal(t, tt.page, repo.params.Page)
			require.Equal(t, tt.size, repo.params.PageSize)
			require.Equal(t, tt.codeType, repo.codeType)
			var body struct {
				Data struct {
					Items []service.RedeemCode `json:"items"`
					Total int                  `json:"total"`
					Page  int                  `json:"page"`
					Size  int                  `json:"page_size"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.NotNil(t, body.Data.Items)
			require.Equal(t, 101, body.Data.Total)
			require.Equal(t, tt.page, body.Data.Page)
			require.Equal(t, tt.size, body.Data.Size)
		})
	}
}
