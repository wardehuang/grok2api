// Package cpaautopproxy is a local extension that keeps CPA auto-proxy slot
// management out of upstream egress/account packages for easier merges.
package cpaautopproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/chenyme/grok2api/backend/internal/transport/http/cpaautopproxy/emailmatch"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

const (
	nodeNamePrefix = "cpa_auto_proxy_"
	// webNodeNameInfix sits between the shared prefix and the slot number so both
	// Console and Web nodes stay discoverable under the same CPA naming family.
	webNodeNameInfix = "web_"
	// accountLookupPageSize uses the repository max page size so one bulk request
	// can index provider emails with as few List round-trips as possible.
	accountLookupPageSize = repository.MaxPageSize
)

// Handler wires the CPA auto-proxy slot API onto the admin surface.
type Handler struct {
	egress   *egressapp.Service
	accounts *accountapp.Service
	logger   *slog.Logger
}

// NewHandler constructs the local CPA auto-proxy handler.
func NewHandler(egressService *egressapp.Service, accountService *accountapp.Service, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{egress: egressService, accounts: accountService, logger: logger}
}

// Register mounts the local routes under an already authenticated admin group.
func (handler *Handler) Register(router *gin.RouterGroup) {
	router.POST("/cpa-auto-proxy/slots", handler.syncSlots)
	router.POST("/cpa-auto-proxy/console-accounts/sync", handler.syncConsoleAccounts)
}

type slotRequest struct {
	Slot     int      `json:"slot"`
	IP       string   `json:"ip"`
	Accounts []string `json:"accounts"`
}

type consoleAccountsSyncRequest struct {
	Emails *[]string `json:"emails"`
}

type slotResult struct {
	Slot                    int      `json:"slot"`
	Action                  string   `json:"action"`
	NodeName                string   `json:"nodeName"`
	NodeID                  string   `json:"nodeId,omitempty"`
	WebNodeName             string   `json:"webNodeName"`
	WebNodeID               string   `json:"webNodeId,omitempty"`
	Assigned                int      `json:"assigned"`
	ConsoleAssigned         int      `json:"consoleAssigned"`
	WebAssigned             int      `json:"webAssigned"`
	OverflowAssigned        int      `json:"overflowAssigned"`
	ConsoleOverflowAssigned int      `json:"consoleOverflowAssigned"`
	WebOverflowAssigned     int      `json:"webOverflowAssigned"`
	OverflowAccounts        []string `json:"overflowAccounts"`
	ConsoleOverflowAccounts []string `json:"consoleOverflowAccounts"`
	WebOverflowAccounts     []string `json:"webOverflowAccounts"`
	SkippedAccounts         []string `json:"skippedAccounts"`
	Error                   string   `json:"error,omitempty"`
}

func consoleNodeName(slot int) string {
	return nodeNamePrefix + strconv.Itoa(slot)
}

func webNodeName(slot int) string {
	return nodeNamePrefix + webNodeNameInfix + strconv.Itoa(slot)
}

func (handler *Handler) syncSlots(ginContext *gin.Context) {
	requestID, _ := ginContext.Get(middleware.RequestIDKey)
	log := handler.logger.With("request_id", requestID, "component", "cpa_auto_proxy")

	var requests []slotRequest
	if err := ginContext.ShouldBindJSON(&requests); err != nil {
		log.Warn("cpa_auto_proxy_sync_rejected", "reason", "invalid_json")
		response.Error(ginContext, http.StatusBadRequest, "invalidRequest", "请求参数无效，需要 slot 数组")
		return
	}
	if len(requests) == 0 {
		log.Warn("cpa_auto_proxy_sync_rejected", "reason", "empty_slots")
		response.Error(ginContext, http.StatusBadRequest, "invalidRequest", "slot 数组不能为空")
		return
	}
	if err := validateSlotRequests(requests); err != nil {
		log.Warn("cpa_auto_proxy_sync_rejected", "reason", "invalid_slots", "error", err.Error(), "slot_count", len(requests))
		response.Error(ginContext, http.StatusBadRequest, "invalidRequest", err.Error())
		return
	}

	log.Info("cpa_auto_proxy_sync_started", "slot_count", len(requests), "slots", slotNumbers(requests), "payload", receivedPayloadSummary(requests))
	for _, requestItem := range requests {
		handler.logSlotReceived(log, requestItem)
	}

	requestContext := ginContext.Request.Context()
	consoleAccounts, err := handler.listProviderAccounts(requestContext, accountdomain.ProviderConsole)
	if err != nil {
		log.Error("cpa_auto_proxy_account_lookup_failed", "provider", string(accountdomain.ProviderConsole), "error", err)
		response.Error(ginContext, http.StatusInternalServerError, "cpaAutoProxyAccountLookupFailed", "读取 Grok Console 账号失败")
		return
	}
	webAccounts, err := handler.listProviderAccounts(requestContext, accountdomain.ProviderWeb)
	if err != nil {
		log.Error("cpa_auto_proxy_account_lookup_failed", "provider", string(accountdomain.ProviderWeb), "error", err)
		response.Error(ginContext, http.StatusInternalServerError, "cpaAutoProxyAccountLookupFailed", "读取 Grok Web 账号失败")
		return
	}
	consoleEmailIndex := emailIndexFromAccounts(consoleAccounts)
	webEmailIndex := emailIndexFromAccounts(webAccounts)

	nodeByName, err := handler.buildNodeNameIndex(requestContext)
	if err != nil {
		log.Error("cpa_auto_proxy_node_lookup_failed", "error", err)
		response.Error(ginContext, http.StatusInternalServerError, "cpaAutoProxyNodeLookupFailed", "读取代理节点失败")
		return
	}

	results := make([]slotResult, 0, len(requests))
	for _, requestItem := range requests {
		result := handler.applySlot(requestContext, requestItem, consoleEmailIndex, webEmailIndex, nodeByName)
		results = append(results, result)
	}

	consoleOverflowAssigned, consoleOverflowError := handler.assignOverflowAccounts(
		requestContext,
		requests,
		results,
		consoleAccounts,
		consoleEmailIndex,
		accountdomain.ProviderConsole,
		func(result slotResult) string { return result.NodeID },
		func(result *slotResult, labels []string) {
			result.ConsoleOverflowAccounts = labels
			result.ConsoleOverflowAssigned = len(labels)
			result.OverflowAccounts = append(result.OverflowAccounts, labels...)
			result.OverflowAssigned += len(labels)
			result.ConsoleAssigned += len(labels)
			result.Assigned += len(labels)
		},
	)
	if consoleOverflowError != nil {
		log.Error(
			"cpa_auto_proxy_overflow_assign_failed",
			"provider", string(accountdomain.ProviderConsole),
			"error", consoleOverflowError,
			"assigned", consoleOverflowAssigned,
		)
	}

	webOverflowAssigned, webOverflowError := handler.assignOverflowAccounts(
		requestContext,
		requests,
		results,
		webAccounts,
		webEmailIndex,
		accountdomain.ProviderWeb,
		func(result slotResult) string { return result.WebNodeID },
		func(result *slotResult, labels []string) {
			result.WebOverflowAccounts = labels
			result.WebOverflowAssigned = len(labels)
			result.OverflowAccounts = append(result.OverflowAccounts, labels...)
			result.OverflowAssigned += len(labels)
			result.WebAssigned += len(labels)
			result.Assigned += len(labels)
		},
	)
	if webOverflowError != nil {
		log.Error(
			"cpa_auto_proxy_overflow_assign_failed",
			"provider", string(accountdomain.ProviderWeb),
			"error", webOverflowError,
			"assigned", webOverflowAssigned,
		)
	}

	for index, requestItem := range requests {
		handler.logSlotResult(log, requestItem, results[index])
	}

	summary := summarizeResults(results)
	log.Info(
		"cpa_auto_proxy_sync_finished",
		"slot_count", len(results),
		"created", summary.created,
		"updated", summary.updated,
		"deleted", summary.deleted,
		"absent", summary.absent,
		"failed", summary.failed,
		"assigned_total", summary.assignedTotal,
		"console_assigned_total", summary.consoleAssignedTotal,
		"web_assigned_total", summary.webAssignedTotal,
		"overflow_assigned_total", summary.overflowAssignedTotal,
		"skipped_total", summary.skippedTotal,
	)
	response.Success(ginContext, http.StatusOK, gin.H{"results": results})
}

func (handler *Handler) syncConsoleAccounts(ginContext *gin.Context) {
	var request consoleAccountsSyncRequest
	if err := ginContext.ShouldBindJSON(&request); err != nil || request.Emails == nil {
		response.Error(ginContext, http.StatusBadRequest, "invalidRequest", "请求参数无效，需要 emails 数组")
		return
	}

	result, err := handler.accounts.SyncConsoleEnabledByEmails(ginContext.Request.Context(), *request.Emails)
	if err != nil {
		if errors.Is(err, accountapp.ErrInvalidInput) {
			response.Error(ginContext, http.StatusBadRequest, "invalidRequest", err.Error())
			return
		}
		requestID, _ := ginContext.Get(middleware.RequestIDKey)
		handler.logger.Error("cpa_auto_proxy_console_account_sync_failed", "request_id", requestID, "error", err, "requested_email_count", len(*request.Emails))
		response.Error(ginContext, http.StatusInternalServerError, "cpaAutoProxyConsoleAccountSyncFailed", "同步 Grok Console 账号状态失败")
		return
	}

	response.Success(ginContext, http.StatusOK, gin.H{
		"total":    result.Total,
		"enabled":  result.Enabled,
		"disabled": result.Disabled,
	})
}

func (handler *Handler) logSlotReceived(log *slog.Logger, requestItem slotRequest) {
	proxyDetail := describeProxyForLog(requestItem.IP)
	log.Info(
		"cpa_auto_proxy_slot_received",
		"slot", requestItem.Slot,
		"node_name", consoleNodeName(requestItem.Slot),
		"web_node_name", webNodeName(requestItem.Slot),
		"proxy_empty", proxyDetail.Empty,
		"proxy_endpoint", proxyDetail.Endpoint,
		"proxy_auth", proxyDetail.HasAuth,
		"proxy_scheme", proxyDetail.Scheme,
		"account_count", len(requestItem.Accounts),
		"accounts", cloneStringSlice(requestItem.Accounts),
	)
}

func (handler *Handler) logSlotResult(log *slog.Logger, requestItem slotRequest, result slotResult) {
	proxyDetail := describeProxyForLog(requestItem.IP)
	matchedAccounts := matchedAccountEmails(requestItem.Accounts, result.SkippedAccounts)
	attrs := []any{
		"slot", result.Slot,
		"action", result.Action,
		"node_name", result.NodeName,
		"web_node_name", result.WebNodeName,
		"proxy_empty", proxyDetail.Empty,
		"proxy_endpoint", proxyDetail.Endpoint,
		"proxy_auth", proxyDetail.HasAuth,
		"proxy_scheme", proxyDetail.Scheme,
		"account_count", len(requestItem.Accounts),
		"accounts", cloneStringSlice(requestItem.Accounts),
		"matched_accounts", matchedAccounts,
		"matched_count", len(matchedAccounts),
		"assigned", result.Assigned,
		"console_assigned", result.ConsoleAssigned,
		"web_assigned", result.WebAssigned,
		"overflow_assigned", result.OverflowAssigned,
		"console_overflow_assigned", result.ConsoleOverflowAssigned,
		"web_overflow_assigned", result.WebOverflowAssigned,
		"overflow_accounts", cloneStringSlice(result.OverflowAccounts),
		"console_overflow_accounts", cloneStringSlice(result.ConsoleOverflowAccounts),
		"web_overflow_accounts", cloneStringSlice(result.WebOverflowAccounts),
		"skipped_count", len(result.SkippedAccounts),
		"skipped_accounts", cloneStringSlice(result.SkippedAccounts),
	}
	if result.NodeID != "" {
		attrs = append(attrs, "node_id", result.NodeID)
	}
	if result.WebNodeID != "" {
		attrs = append(attrs, "web_node_id", result.WebNodeID)
	}
	if result.Error != "" {
		attrs = append(attrs, "error", result.Error)
		log.Error("cpa_auto_proxy_slot_failed", attrs...)
		return
	}
	log.Info("cpa_auto_proxy_slot_applied", attrs...)
}

type resultSummary struct {
	created               int
	updated               int
	deleted               int
	absent                int
	failed                int
	assignedTotal         int
	consoleAssignedTotal  int
	webAssignedTotal      int
	overflowAssignedTotal int
	skippedTotal          int
}

func summarizeResults(results []slotResult) resultSummary {
	summary := resultSummary{}
	for _, result := range results {
		switch result.Action {
		case "created":
			summary.created++
		case "updated":
			summary.updated++
		case "deleted":
			summary.deleted++
		case "absent":
			summary.absent++
		case "failed":
			summary.failed++
		}
		summary.assignedTotal += result.Assigned
		summary.consoleAssignedTotal += result.ConsoleAssigned
		summary.webAssignedTotal += result.WebAssigned
		summary.overflowAssignedTotal += result.OverflowAssigned
		summary.skippedTotal += len(result.SkippedAccounts)
	}
	return summary
}

func slotNumbers(requests []slotRequest) []int {
	values := make([]int, 0, len(requests))
	for _, requestItem := range requests {
		values = append(values, requestItem.Slot)
	}
	return values
}

type receivedSlotSummary struct {
	Slot          int      `json:"slot"`
	ProxyEmpty    bool     `json:"proxyEmpty"`
	ProxyEndpoint string   `json:"proxyEndpoint,omitempty"`
	ProxyAuth     bool     `json:"proxyAuth"`
	ProxyScheme   string   `json:"proxyScheme,omitempty"`
	AccountCount  int      `json:"accountCount"`
	Accounts      []string `json:"accounts"`
}

func receivedPayloadSummary(requests []slotRequest) []receivedSlotSummary {
	summaries := make([]receivedSlotSummary, 0, len(requests))
	for _, requestItem := range requests {
		proxyDetail := describeProxyForLog(requestItem.IP)
		summaries = append(summaries, receivedSlotSummary{
			Slot:          requestItem.Slot,
			ProxyEmpty:    proxyDetail.Empty,
			ProxyEndpoint: proxyDetail.Endpoint,
			ProxyAuth:     proxyDetail.HasAuth,
			ProxyScheme:   proxyDetail.Scheme,
			AccountCount:  len(requestItem.Accounts),
			Accounts:      cloneStringSlice(requestItem.Accounts),
		})
	}
	return summaries
}

type proxyLogDetail struct {
	Empty    bool
	Endpoint string
	HasAuth  bool
	Scheme   string
}

// describeProxyForLog keeps scheme/host/port for troubleshooting and never writes userinfo.
func describeProxyForLog(rawProxyURL string) proxyLogDetail {
	value := strings.TrimSpace(rawProxyURL)
	if value == "" {
		return proxyLogDetail{Empty: true}
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return proxyLogDetail{Endpoint: "(unparseable)"}
	}
	return proxyLogDetail{
		Endpoint: parsed.Scheme + "://" + parsed.Host,
		HasAuth:  parsed.User != nil,
		Scheme:   strings.ToLower(parsed.Scheme),
	}
}

func matchedAccountEmails(accountEmails, skippedAccounts []string) []string {
	skippedSet := make(map[string]struct{}, len(skippedAccounts))
	for _, skippedEmail := range skippedAccounts {
		skippedSet[strings.ToLower(strings.TrimSpace(skippedEmail))] = struct{}{}
	}
	matched := make([]string, 0, len(accountEmails))
	seen := make(map[string]struct{}, len(accountEmails))
	for _, rawEmail := range accountEmails {
		trimmedEmail := strings.TrimSpace(rawEmail)
		normalizedEmail := strings.ToLower(trimmedEmail)
		if trimmedEmail == "" {
			continue
		}
		if _, skipped := skippedSet[normalizedEmail]; skipped {
			continue
		}
		if _, exists := seen[normalizedEmail]; exists {
			continue
		}
		seen[normalizedEmail] = struct{}{}
		matched = append(matched, trimmedEmail)
	}
	return matched
}

func cloneStringSlice(values []string) []string {
	if values == nil {
		return []string{}
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func validateSlotRequests(requests []slotRequest) error {
	seenSlots := make(map[int]struct{}, len(requests))
	for index, requestItem := range requests {
		if requestItem.Slot < 0 {
			return fmt.Errorf("第 %d 项 slot 不能为负数", index)
		}
		consoleName := consoleNodeName(requestItem.Slot)
		webName := webNodeName(requestItem.Slot)
		if len(consoleName) > 160 || len(webName) > 160 {
			return fmt.Errorf("第 %d 项 slot 过大，节点名超过 160 字符", index)
		}
		if _, exists := seenSlots[requestItem.Slot]; exists {
			return fmt.Errorf("slot %d 在同一请求中重复出现", requestItem.Slot)
		}
		seenSlots[requestItem.Slot] = struct{}{}
		for accountIndex, accountEmail := range requestItem.Accounts {
			if strings.TrimSpace(accountEmail) == "" {
				return fmt.Errorf("slot %d 的 accounts[%d] 不能为空", requestItem.Slot, accountIndex)
			}
		}
	}
	return nil
}

func (handler *Handler) applySlot(
	requestContext context.Context,
	requestItem slotRequest,
	consoleEmailIndex map[string][]uint64,
	webEmailIndex map[string][]uint64,
	nodeByName map[string]egressdomain.PublicNode,
) slotResult {
	consoleName := consoleNodeName(requestItem.Slot)
	webName := webNodeName(requestItem.Slot)
	result := slotResult{
		Slot:                    requestItem.Slot,
		NodeName:                consoleName,
		WebNodeName:             webName,
		OverflowAccounts:        []string{},
		ConsoleOverflowAccounts: []string{},
		WebOverflowAccounts:     []string{},
		SkippedAccounts:         []string{},
	}

	proxyAddress := strings.TrimSpace(requestItem.IP)
	if proxyAddress == "" {
		return handler.deleteSlotNodes(requestContext, requestItem, result, consoleEmailIndex, webEmailIndex, nodeByName)
	}

	consoleNode, consoleAction, consoleError := handler.upsertScopedNode(
		requestContext,
		consoleName,
		egressdomain.ScopeConsole,
		proxyAddress,
		nodeByName,
	)
	if consoleError != nil {
		result.Action = "failed"
		result.Error = consoleError.Error()
		return result
	}
	result.NodeID = strconv.FormatUint(consoleNode.ID, 10)

	webNode, webAction, webError := handler.upsertScopedNode(
		requestContext,
		webName,
		egressdomain.ScopeWeb,
		proxyAddress,
		nodeByName,
	)
	if webError != nil {
		result.Action = "failed"
		result.Error = webError.Error()
		return result
	}
	result.WebNodeID = strconv.FormatUint(webNode.ID, 10)
	result.Action = mergeUpsertActions(consoleAction, webAction)

	consoleAccountIDs := resolveProviderAccountIDs(requestItem.Accounts, consoleEmailIndex)
	webAccountIDs := resolveProviderAccountIDs(requestItem.Accounts, webEmailIndex)
	result.SkippedAccounts = skippedEmails(requestItem.Accounts, consoleEmailIndex, webEmailIndex)

	if len(consoleAccountIDs) > 0 {
		assignmentResult, assignError := handler.egress.AssignAccounts(
			requestContext,
			consoleNode.ID,
			accountdomain.ProviderConsole,
			consoleAccountIDs,
			accountdomain.EgressAssignmentManual,
		)
		if assignError != nil {
			result.Action = "failed"
			result.Error = assignError.Error()
			return result
		}
		result.ConsoleAssigned = assignmentResult.Assigned
		result.Assigned += assignmentResult.Assigned
	}

	if len(webAccountIDs) > 0 {
		assignmentResult, assignError := handler.egress.AssignAccounts(
			requestContext,
			webNode.ID,
			accountdomain.ProviderWeb,
			webAccountIDs,
			accountdomain.EgressAssignmentManual,
		)
		if assignError != nil {
			result.Action = "failed"
			result.Error = assignError.Error()
			return result
		}
		result.WebAssigned = assignmentResult.Assigned
		result.Assigned += assignmentResult.Assigned
	}

	return result
}

func (handler *Handler) deleteSlotNodes(
	requestContext context.Context,
	requestItem slotRequest,
	result slotResult,
	consoleEmailIndex map[string][]uint64,
	webEmailIndex map[string][]uint64,
	nodeByName map[string]egressdomain.PublicNode,
) slotResult {
	result.SkippedAccounts = skippedEmails(requestItem.Accounts, consoleEmailIndex, webEmailIndex)

	consoleDeleted, consoleNodeID, consoleError := handler.deleteNamedNode(requestContext, result.NodeName, nodeByName)
	if consoleError != nil {
		result.Action = "failed"
		result.Error = consoleError.Error()
		return result
	}
	if consoleNodeID != "" {
		result.NodeID = consoleNodeID
	}

	webDeleted, webNodeID, webError := handler.deleteNamedNode(requestContext, result.WebNodeName, nodeByName)
	if webError != nil {
		result.Action = "failed"
		result.Error = webError.Error()
		return result
	}
	if webNodeID != "" {
		result.WebNodeID = webNodeID
	}

	if consoleDeleted || webDeleted {
		result.Action = "deleted"
		return result
	}
	result.Action = "absent"
	return result
}

func (handler *Handler) deleteNamedNode(
	requestContext context.Context,
	nodeName string,
	nodeByName map[string]egressdomain.PublicNode,
) (deleted bool, nodeID string, err error) {
	existingNode, exists := nodeByName[nodeName]
	if !exists {
		return false, "", nil
	}
	if deleteError := handler.egress.Delete(requestContext, existingNode.ID); deleteError != nil {
		if errors.Is(deleteError, egressapp.ErrNotFound) {
			delete(nodeByName, nodeName)
			return false, strconv.FormatUint(existingNode.ID, 10), nil
		}
		return false, strconv.FormatUint(existingNode.ID, 10), deleteError
	}
	delete(nodeByName, nodeName)
	return true, strconv.FormatUint(existingNode.ID, 10), nil
}

func (handler *Handler) upsertScopedNode(
	requestContext context.Context,
	nodeName string,
	scope egressdomain.Scope,
	proxyAddress string,
	nodeByName map[string]egressdomain.PublicNode,
) (egressdomain.PublicNode, string, error) {
	proxyPoolEnabled := false
	unlimitedAccountCapacity := 0
	nodeInput := egressapp.Input{
		Name:            nodeName,
		Scope:           scope,
		Enabled:         true,
		ProxyPool:       &proxyPoolEnabled,
		AccountCapacity: &unlimitedAccountCapacity,
		ProxyURL:        &proxyAddress,
	}

	if existingNode, exists := nodeByName[nodeName]; exists {
		publicNode, updateError := handler.egress.Update(requestContext, existingNode.ID, nodeInput)
		if updateError != nil {
			return egressdomain.PublicNode{}, "", updateError
		}
		nodeByName[nodeName] = publicNode
		return publicNode, "updated", nil
	}

	publicNode, createError := handler.egress.Create(requestContext, nodeInput)
	if createError != nil {
		return egressdomain.PublicNode{}, "", createError
	}
	nodeByName[nodeName] = publicNode
	return publicNode, "created", nil
}

func mergeUpsertActions(consoleAction, webAction string) string {
	if consoleAction == "updated" || webAction == "updated" {
		return "updated"
	}
	return "created"
}

func (handler *Handler) buildNodeNameIndex(requestContext context.Context) (map[string]egressdomain.PublicNode, error) {
	nodes, err := handler.egress.ListAll(requestContext, "", repository.SortQuery{})
	if err != nil {
		return nil, err
	}
	index := make(map[string]egressdomain.PublicNode, len(nodes))
	for _, node := range nodes {
		if !strings.HasPrefix(node.Name, nodeNamePrefix) {
			continue
		}
		// Later duplicates keep the latest row so repeated syncs remain deterministic
		// even if historical manual copies of the same name exist.
		index[node.Name] = node
	}
	return index, nil
}

func (handler *Handler) listProviderAccounts(requestContext context.Context, provider accountdomain.Provider) ([]accountRef, error) {
	accounts := make([]accountRef, 0)
	page := 1
	for {
		views, total, err := handler.accounts.List(
			requestContext,
			page,
			accountLookupPageSize,
			"",
			accountapp.ListFilter{Provider: string(provider)},
		)
		if err != nil {
			return nil, err
		}
		for _, view := range views {
			accounts = append(accounts, accountRef{
				ID:    view.Credential.ID,
				Name:  strings.TrimSpace(view.Credential.Name),
				Email: strings.ToLower(strings.TrimSpace(view.Credential.Email)),
			})
		}
		if int64(page*accountLookupPageSize) >= total || len(views) == 0 {
			break
		}
		page++
	}
	return accounts, nil
}

func emailIndexFromAccounts(accounts []accountRef) map[string][]uint64 {
	index := make(map[string][]uint64)
	for _, account := range accounts {
		if account.Email == "" {
			continue
		}
		// Index both the literal lowercased email and the Gmail-canonical key so
		// lucymunen80@gmail.com matches a stored lucymunen8.0@gmail.com.
		matchKeys := emailMatchKeys(account.Email)
		for _, matchKey := range matchKeys {
			index[matchKey] = append(index[matchKey], account.ID)
		}
	}
	return index
}

// emailMatchKeys delegates to the shared package so slots and account state
// sync use one identical matching implementation.
func emailMatchKeys(rawEmail string) []string {
	return emailmatch.MatchKeys(rawEmail)
}

type accountRef struct {
	ID    uint64
	Name  string
	Email string
}

func (handler *Handler) assignOverflowAccounts(
	requestContext context.Context,
	requests []slotRequest,
	results []slotResult,
	providerAccounts []accountRef,
	providerEmailIndex map[string][]uint64,
	provider accountdomain.Provider,
	nodeIDFromResult func(slotResult) string,
	applyOverflowLabels func(result *slotResult, labels []string),
) (int, error) {
	claimedAccountIDs := make(map[uint64]struct{})
	for _, requestItem := range requests {
		accountIDs := resolveProviderAccountIDs(requestItem.Accounts, providerEmailIndex)
		for _, accountID := range accountIDs {
			claimedAccountIDs[accountID] = struct{}{}
		}
	}

	activeIndexes := make([]int, 0)
	for index, result := range results {
		if result.Action == "failed" || result.Action == "deleted" || result.Action == "absent" {
			continue
		}
		if nodeIDFromResult(result) == "" {
			continue
		}
		activeIndexes = append(activeIndexes, index)
	}
	sort.SliceStable(activeIndexes, func(left, right int) bool {
		return results[activeIndexes[left]].Slot < results[activeIndexes[right]].Slot
	})
	if len(activeIndexes) == 0 {
		return 0, nil
	}

	overflowAccounts := leftoverAccounts(providerAccounts, claimedAccountIDs)
	if len(overflowAccounts) == 0 {
		return 0, nil
	}

	assignmentsByNode := make(map[uint64][]uint64)
	labelsByResultIndex := make(map[int][]string)
	for overflowIndex, overflowAccount := range overflowAccounts {
		resultIndex := activeIndexes[overflowIndex%len(activeIndexes)]
		nodeID, parseError := strconv.ParseUint(nodeIDFromResult(results[resultIndex]), 10, 64)
		if parseError != nil || nodeID == 0 {
			continue
		}
		assignmentsByNode[nodeID] = append(assignmentsByNode[nodeID], overflowAccount.ID)
		labelsByResultIndex[resultIndex] = append(labelsByResultIndex[resultIndex], overflowAccountLabel(overflowAccount))
	}

	assignedTotal := 0
	for nodeID, accountIDs := range assignmentsByNode {
		assignmentResult, assignError := handler.egress.AssignAccounts(
			requestContext,
			nodeID,
			provider,
			accountIDs,
			accountdomain.EgressAssignmentManual,
		)
		if assignError != nil {
			return assignedTotal, assignError
		}
		assignedTotal += assignmentResult.Assigned
	}
	for resultIndex, labels := range labelsByResultIndex {
		applyOverflowLabels(&results[resultIndex], labels)
	}
	return assignedTotal, nil
}

func leftoverAccounts(accounts []accountRef, claimedAccountIDs map[uint64]struct{}) []accountRef {
	leftovers := make([]accountRef, 0)
	for _, account := range accounts {
		if _, claimed := claimedAccountIDs[account.ID]; claimed {
			continue
		}
		leftovers = append(leftovers, account)
	}
	sort.SliceStable(leftovers, func(left, right int) bool {
		leftName := strings.ToLower(leftovers[left].Name)
		rightName := strings.ToLower(leftovers[right].Name)
		if leftName != rightName {
			return leftName < rightName
		}
		if leftovers[left].Email != leftovers[right].Email {
			return leftovers[left].Email < leftovers[right].Email
		}
		return leftovers[left].ID < leftovers[right].ID
	})
	return leftovers
}

func overflowAccountLabel(account accountRef) string {
	if account.Name != "" {
		return account.Name
	}
	if account.Email != "" {
		return account.Email
	}
	return strconv.FormatUint(account.ID, 10)
}

func resolveProviderAccountIDs(accountEmails []string, providerEmailIndex map[string][]uint64) []uint64 {
	seenAccountIDs := make(map[uint64]struct{})
	accountIDs := make([]uint64, 0)
	for _, rawEmail := range accountEmails {
		for _, accountID := range lookupProviderAccountIDs(rawEmail, providerEmailIndex) {
			if _, exists := seenAccountIDs[accountID]; exists {
				continue
			}
			seenAccountIDs[accountID] = struct{}{}
			accountIDs = append(accountIDs, accountID)
		}
	}
	return accountIDs
}

func lookupProviderAccountIDs(rawEmail string, providerEmailIndex map[string][]uint64) []uint64 {
	seenAccountIDs := make(map[uint64]struct{})
	matchedIDs := make([]uint64, 0)
	for _, matchKey := range emailMatchKeys(rawEmail) {
		for _, accountID := range providerEmailIndex[matchKey] {
			if _, exists := seenAccountIDs[accountID]; exists {
				continue
			}
			seenAccountIDs[accountID] = struct{}{}
			matchedIDs = append(matchedIDs, accountID)
		}
	}
	return matchedIDs
}

func skippedEmails(accountEmails []string, consoleEmailIndex, webEmailIndex map[string][]uint64) []string {
	skippedAccounts := make([]string, 0)
	for _, rawEmail := range accountEmails {
		consoleMatches := lookupProviderAccountIDs(rawEmail, consoleEmailIndex)
		webMatches := lookupProviderAccountIDs(rawEmail, webEmailIndex)
		if len(consoleMatches) == 0 && len(webMatches) == 0 {
			skippedAccounts = append(skippedAccounts, strings.TrimSpace(rawEmail))
		}
	}
	return skippedAccounts
}
