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
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

const (
	nodeNamePrefix = "cpa_auto_proxy_"
	// accountLookupPageSize uses the repository max page size so one bulk request
	// can index Console emails with as few List round-trips as possible.
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
}

type slotRequest struct {
	Slot     int      `json:"slot"`
	IP       string   `json:"ip"`
	Accounts []string `json:"accounts"`
}

type slotResult struct {
	Slot              int      `json:"slot"`
	Action            string   `json:"action"`
	NodeName          string   `json:"nodeName"`
	NodeID            string   `json:"nodeId,omitempty"`
	Assigned          int      `json:"assigned"`
	OverflowAssigned  int      `json:"overflowAssigned"`
	OverflowAccounts  []string `json:"overflowAccounts"`
	SkippedAccounts   []string `json:"skippedAccounts"`
	Error             string   `json:"error,omitempty"`
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
	consoleAccounts, err := handler.listConsoleAccounts(requestContext)
	if err != nil {
		log.Error("cpa_auto_proxy_account_lookup_failed", "error", err)
		response.Error(ginContext, http.StatusInternalServerError, "cpaAutoProxyAccountLookupFailed", "读取 Grok Console 账号失败")
		return
	}
	consoleEmailIndex := emailIndexFromAccounts(consoleAccounts)

	nodeByName, err := handler.buildNodeNameIndex(requestContext)
	if err != nil {
		log.Error("cpa_auto_proxy_node_lookup_failed", "error", err)
		response.Error(ginContext, http.StatusInternalServerError, "cpaAutoProxyNodeLookupFailed", "读取代理节点失败")
		return
	}

	results := make([]slotResult, 0, len(requests))
	for _, requestItem := range requests {
		result := handler.applySlot(requestContext, requestItem, consoleEmailIndex, nodeByName)
		results = append(results, result)
	}

	overflowAssigned, overflowError := handler.assignOverflowAccounts(requestContext, requests, results, consoleAccounts, consoleEmailIndex)
	if overflowError != nil {
		log.Error("cpa_auto_proxy_overflow_assign_failed", "error", overflowError, "assigned", overflowAssigned)
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
		"overflow_assigned_total", summary.overflowAssignedTotal,
		"skipped_total", summary.skippedTotal,
	)
	response.Success(ginContext, http.StatusOK, gin.H{"results": results})
}

func (handler *Handler) logSlotReceived(log *slog.Logger, requestItem slotRequest) {
	proxyDetail := describeProxyForLog(requestItem.IP)
	log.Info(
		"cpa_auto_proxy_slot_received",
		"slot", requestItem.Slot,
		"node_name", nodeNamePrefix+strconv.Itoa(requestItem.Slot),
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
		"proxy_empty", proxyDetail.Empty,
		"proxy_endpoint", proxyDetail.Endpoint,
		"proxy_auth", proxyDetail.HasAuth,
		"proxy_scheme", proxyDetail.Scheme,
		"account_count", len(requestItem.Accounts),
		"accounts", cloneStringSlice(requestItem.Accounts),
		"matched_accounts", matchedAccounts,
		"matched_count", len(matchedAccounts),
		"assigned", result.Assigned,
		"overflow_assigned", result.OverflowAssigned,
		"overflow_accounts", cloneStringSlice(result.OverflowAccounts),
		"skipped_count", len(result.SkippedAccounts),
		"skipped_accounts", cloneStringSlice(result.SkippedAccounts),
	}
	if result.NodeID != "" {
		attrs = append(attrs, "node_id", result.NodeID)
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
	Slot         int      `json:"slot"`
	ProxyEmpty   bool     `json:"proxyEmpty"`
	ProxyEndpoint string  `json:"proxyEndpoint,omitempty"`
	ProxyAuth    bool     `json:"proxyAuth"`
	ProxyScheme  string   `json:"proxyScheme,omitempty"`
	AccountCount int      `json:"accountCount"`
	Accounts     []string `json:"accounts"`
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
		nodeName := nodeNamePrefix + strconv.Itoa(requestItem.Slot)
		if len(nodeName) > 160 {
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
	nodeByName map[string]egressdomain.PublicNode,
) slotResult {
	nodeName := nodeNamePrefix + strconv.Itoa(requestItem.Slot)
	result := slotResult{
		Slot:             requestItem.Slot,
		NodeName:         nodeName,
		OverflowAccounts: []string{},
		SkippedAccounts:  []string{},
	}

	proxyAddress := strings.TrimSpace(requestItem.IP)
	if proxyAddress == "" {
		existingNode, exists := nodeByName[nodeName]
		if !exists {
			result.Action = "absent"
			result.SkippedAccounts = skippedEmails(requestItem.Accounts, consoleEmailIndex)
			return result
		}
		if err := handler.egress.Delete(requestContext, existingNode.ID); err != nil {
			if errors.Is(err, egressapp.ErrNotFound) {
				delete(nodeByName, nodeName)
				result.Action = "absent"
				result.SkippedAccounts = skippedEmails(requestItem.Accounts, consoleEmailIndex)
				return result
			}
			result.Action = "failed"
			result.Error = err.Error()
			return result
		}
		delete(nodeByName, nodeName)
		result.Action = "deleted"
		result.NodeID = strconv.FormatUint(existingNode.ID, 10)
		result.SkippedAccounts = skippedEmails(requestItem.Accounts, consoleEmailIndex)
		return result
	}

	proxyPoolEnabled := false
	unlimitedAccountCapacity := 0
	nodeInput := egressapp.Input{
		Name:            nodeName,
		Scope:           egressdomain.ScopeConsole,
		Enabled:         true,
		ProxyPool:       &proxyPoolEnabled,
		AccountCapacity: &unlimitedAccountCapacity,
		ProxyURL:        &proxyAddress,
	}

	var (
		publicNode egressdomain.PublicNode
		applyError error
	)
	if existingNode, exists := nodeByName[nodeName]; exists {
		publicNode, applyError = handler.egress.Update(requestContext, existingNode.ID, nodeInput)
		result.Action = "updated"
	} else {
		publicNode, applyError = handler.egress.Create(requestContext, nodeInput)
		result.Action = "created"
	}
	if applyError != nil {
		result.Action = "failed"
		result.Error = applyError.Error()
		return result
	}
	nodeByName[nodeName] = publicNode
	result.NodeID = strconv.FormatUint(publicNode.ID, 10)

	accountIDs, skippedAccounts := resolveConsoleAccountIDs(requestItem.Accounts, consoleEmailIndex)
	result.SkippedAccounts = skippedAccounts
	if len(accountIDs) == 0 {
		return result
	}

	assignmentResult, assignError := handler.egress.AssignAccounts(
		requestContext,
		publicNode.ID,
		accountdomain.ProviderConsole,
		accountIDs,
		accountdomain.EgressAssignmentManual,
	)
	if assignError != nil {
		result.Action = "failed"
		result.Error = assignError.Error()
		return result
	}
	result.Assigned = assignmentResult.Assigned
	return result
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

func (handler *Handler) listConsoleAccounts(requestContext context.Context) ([]consoleAccountRef, error) {
	accounts := make([]consoleAccountRef, 0)
	page := 1
	for {
		views, total, err := handler.accounts.List(
			requestContext,
			page,
			accountLookupPageSize,
			"",
			accountapp.ListFilter{Provider: string(accountdomain.ProviderConsole)},
		)
		if err != nil {
			return nil, err
		}
		for _, view := range views {
			accounts = append(accounts, consoleAccountRef{
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

func emailIndexFromAccounts(accounts []consoleAccountRef) map[string][]uint64 {
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

// emailMatchKeys returns lookup keys for an address. Gmail/Googlemail local-parts
// ignore dots and +tags, so those variants collapse to one canonical key.
func emailMatchKeys(rawEmail string) []string {
	normalizedEmail := strings.ToLower(strings.TrimSpace(rawEmail))
	if normalizedEmail == "" {
		return nil
	}
	keys := []string{normalizedEmail}
	if canonicalKey := gmailCanonicalEmail(normalizedEmail); canonicalKey != "" && canonicalKey != normalizedEmail {
		keys = append(keys, canonicalKey)
	}
	return keys
}

func gmailCanonicalEmail(normalizedEmail string) string {
	atIndex := strings.LastIndex(normalizedEmail, "@")
	if atIndex <= 0 || atIndex == len(normalizedEmail)-1 {
		return ""
	}
	localPart := normalizedEmail[:atIndex]
	domain := normalizedEmail[atIndex+1:]
	switch domain {
	case "gmail.com", "googlemail.com":
	default:
		return ""
	}
	if plusIndex := strings.IndexByte(localPart, '+'); plusIndex >= 0 {
		localPart = localPart[:plusIndex]
	}
	localPart = strings.ReplaceAll(localPart, ".", "")
	if localPart == "" {
		return ""
	}
	return localPart + "@gmail.com"
}

type consoleAccountRef struct {
	ID    uint64
	Name  string
	Email string
}

func (handler *Handler) assignOverflowAccounts(
	requestContext context.Context,
	requests []slotRequest,
	results []slotResult,
	consoleAccounts []consoleAccountRef,
	consoleEmailIndex map[string][]uint64,
) (int, error) {
	claimedAccountIDs := make(map[uint64]struct{})
	for _, requestItem := range requests {
		accountIDs, _ := resolveConsoleAccountIDs(requestItem.Accounts, consoleEmailIndex)
		for _, accountID := range accountIDs {
			claimedAccountIDs[accountID] = struct{}{}
		}
	}

	activeIndexes := make([]int, 0)
	for index, result := range results {
		if result.NodeID == "" || result.Action == "failed" || result.Action == "deleted" || result.Action == "absent" {
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

	overflowAccounts := leftoverConsoleAccounts(consoleAccounts, claimedAccountIDs)
	if len(overflowAccounts) == 0 {
		return 0, nil
	}

	assignmentsByNode := make(map[uint64][]uint64)
	labelsByResultIndex := make(map[int][]string)
	for overflowIndex, overflowAccount := range overflowAccounts {
		resultIndex := activeIndexes[overflowIndex%len(activeIndexes)]
		nodeID, parseError := strconv.ParseUint(results[resultIndex].NodeID, 10, 64)
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
			accountdomain.ProviderConsole,
			accountIDs,
			accountdomain.EgressAssignmentManual,
		)
		if assignError != nil {
			return assignedTotal, assignError
		}
		assignedTotal += assignmentResult.Assigned
	}
	for resultIndex, labels := range labelsByResultIndex {
		results[resultIndex].OverflowAccounts = labels
		results[resultIndex].OverflowAssigned = len(labels)
		results[resultIndex].Assigned += len(labels)
	}
	return assignedTotal, nil
}

func leftoverConsoleAccounts(accounts []consoleAccountRef, claimedAccountIDs map[uint64]struct{}) []consoleAccountRef {
	leftovers := make([]consoleAccountRef, 0)
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

func overflowAccountLabel(account consoleAccountRef) string {
	if account.Name != "" {
		return account.Name
	}
	if account.Email != "" {
		return account.Email
	}
	return strconv.FormatUint(account.ID, 10)
}

func resolveConsoleAccountIDs(accountEmails []string, consoleEmailIndex map[string][]uint64) (accountIDs []uint64, skippedAccounts []string) {
	seenAccountIDs := make(map[uint64]struct{})
	skippedAccounts = make([]string, 0)
	for _, rawEmail := range accountEmails {
		matchedIDs := lookupConsoleAccountIDs(rawEmail, consoleEmailIndex)
		if len(matchedIDs) == 0 {
			skippedAccounts = append(skippedAccounts, strings.TrimSpace(rawEmail))
			continue
		}
		for _, accountID := range matchedIDs {
			if _, exists := seenAccountIDs[accountID]; exists {
				continue
			}
			seenAccountIDs[accountID] = struct{}{}
			accountIDs = append(accountIDs, accountID)
		}
	}
	return accountIDs, skippedAccounts
}

func lookupConsoleAccountIDs(rawEmail string, consoleEmailIndex map[string][]uint64) []uint64 {
	seenAccountIDs := make(map[uint64]struct{})
	matchedIDs := make([]uint64, 0)
	for _, matchKey := range emailMatchKeys(rawEmail) {
		for _, accountID := range consoleEmailIndex[matchKey] {
			if _, exists := seenAccountIDs[accountID]; exists {
				continue
			}
			seenAccountIDs[accountID] = struct{}{}
			matchedIDs = append(matchedIDs, accountID)
		}
	}
	return matchedIDs
}

func skippedEmails(accountEmails []string, consoleEmailIndex map[string][]uint64) []string {
	_, skippedAccounts := resolveConsoleAccountIDs(accountEmails, consoleEmailIndex)
	return skippedAccounts
}
