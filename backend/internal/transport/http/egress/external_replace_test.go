package egress

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestReplaceWebConsoleProxiesRejectsNonArrayBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/egress-nodes/replace", strings.NewReader(`{"ips":[]}`))
	context.Request.Header.Set("Content-Type", "application/json")
	NewHandler(nil).replaceWebConsoleProxies(context)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"invalidRequest"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
