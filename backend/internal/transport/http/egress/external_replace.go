package egress

import (
	"net/http"

	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

func (h *Handler) RegisterExternal(router *gin.RouterGroup) {
	router.POST("/egress-nodes/replace", h.replaceWebConsoleProxies)
}

type externalNodeResponse struct {
	ID    uint64 `json:"id,string"`
	Name  string `json:"name"`
	Scope string `json:"scope"`
}

func (h *Handler) replaceWebConsoleProxies(c *gin.Context) {
	var proxyURLs []string
	if c.ShouldBindJSON(&proxyURLs) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.ReplaceWebConsoleProxies(c.Request.Context(), proxyURLs)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"deleted":         value.Deleted,
		"created":         value.Created,
		"webAssigned":     value.WebAssigned,
		"consoleAssigned": value.ConsoleAssigned,
		"webNodes":        newExternalNodeResponses(value.WebNodes),
		"consoleNodes":    newExternalNodeResponses(value.ConsoleNodes),
	})
}

func newExternalNodeResponses(values []egressdomain.PublicNode) []externalNodeResponse {
	items := make([]externalNodeResponse, 0, len(values))
	for _, value := range values {
		items = append(items, externalNodeResponse{ID: value.ID, Name: value.Name, Scope: string(value.Scope)})
	}
	return items
}
