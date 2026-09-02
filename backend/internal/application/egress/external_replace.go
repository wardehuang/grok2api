package egress

import (
	"context"
	"fmt"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	maxExternalProxyURLs          = 200
	externalWebNodeNamePrefix     = "external_web_"
	externalConsoleNodeNamePrefix = "external_console_"
)

// ReplaceWebConsoleResult is the public outcome of a full Web/Console proxy replacement.
type ReplaceWebConsoleResult struct {
	Deleted         int
	Created         int
	WebAssigned     int
	ConsoleAssigned int
	WebNodes        []domain.PublicNode
	ConsoleNodes    []domain.PublicNode
}

// ReplaceWebConsoleProxies deletes every grok_web and grok_console egress node,
// creates one Web node and one Console node per proxy URL, then round-robins
// all accounts in those two pools onto the new nodes as manual bindings.
// Build, web-asset, and console-asset nodes are left untouched.
func (s *Service) ReplaceWebConsoleProxies(ctx context.Context, proxyURLs []string) (ReplaceWebConsoleResult, error) {
	urls, err := normalizeExternalProxyURLs(proxyURLs)
	if err != nil {
		return ReplaceWebConsoleResult{}, err
	}
	if s.accounts == nil {
		return ReplaceWebConsoleResult{}, ErrOperationsUnavailable
	}

	s.assignmentMu.Lock()
	defer s.assignmentMu.Unlock()

	oldIDs, err := s.listWebConsoleNodeIDs(ctx)
	if err != nil {
		return ReplaceWebConsoleResult{}, err
	}

	deleted := 0
	if len(oldIDs) > 0 {
		deleted, err = s.DeleteMany(ctx, oldIDs)
		if err != nil {
			return ReplaceWebConsoleResult{}, err
		}
	}

	proxyPool := false
	capacity := 0
	webNodes := make([]domain.PublicNode, 0, len(urls))
	consoleNodes := make([]domain.PublicNode, 0, len(urls))
	webIDs := make([]uint64, 0, len(urls))
	consoleIDs := make([]uint64, 0, len(urls))
	for index, proxyURL := range urls {
		urlCopy := proxyURL
		web, createErr := s.Create(ctx, Input{
			Name: fmt.Sprintf("%s%d", externalWebNodeNamePrefix, index), Scope: domain.ScopeWeb,
			Enabled: true, ProxyPool: &proxyPool, AccountCapacity: &capacity, ProxyURL: &urlCopy,
		})
		if createErr != nil {
			return ReplaceWebConsoleResult{Deleted: deleted, Created: len(webNodes) + len(consoleNodes)}, createErr
		}
		webNodes = append(webNodes, web)
		webIDs = append(webIDs, web.ID)

		console, createErr := s.Create(ctx, Input{
			Name: fmt.Sprintf("%s%d", externalConsoleNodeNamePrefix, index), Scope: domain.ScopeConsole,
			Enabled: true, ProxyPool: &proxyPool, AccountCapacity: &capacity, ProxyURL: &urlCopy,
		})
		if createErr != nil {
			return ReplaceWebConsoleResult{Deleted: deleted, Created: len(webNodes) + len(consoleNodes)}, createErr
		}
		consoleNodes = append(consoleNodes, console)
		consoleIDs = append(consoleIDs, console.ID)
	}

	webAssigned, err := s.roundRobinAssignAll(ctx, accountdomain.ProviderWeb, webIDs)
	if err != nil {
		return ReplaceWebConsoleResult{
			Deleted: deleted, Created: len(webNodes) + len(consoleNodes),
			WebAssigned: webAssigned, WebNodes: webNodes, ConsoleNodes: consoleNodes,
		}, err
	}
	consoleAssigned, err := s.roundRobinAssignAll(ctx, accountdomain.ProviderConsole, consoleIDs)
	if err != nil {
		return ReplaceWebConsoleResult{
			Deleted: deleted, Created: len(webNodes) + len(consoleNodes),
			WebAssigned: webAssigned, ConsoleAssigned: consoleAssigned,
			WebNodes: webNodes, ConsoleNodes: consoleNodes,
		}, err
	}

	return ReplaceWebConsoleResult{
		Deleted: deleted, Created: len(webNodes) + len(consoleNodes),
		WebAssigned: webAssigned, ConsoleAssigned: consoleAssigned,
		WebNodes: webNodes, ConsoleNodes: consoleNodes,
	}, nil
}

func (s *Service) listWebConsoleNodeIDs(ctx context.Context) ([]uint64, error) {
	web, err := s.repository.ListEgressNodes(ctx, domain.ScopeWeb, repository.SortQuery{})
	if err != nil {
		return nil, err
	}
	console, err := s.repository.ListEgressNodes(ctx, domain.ScopeConsole, repository.SortQuery{})
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(web)+len(console))
	for _, node := range web {
		ids = append(ids, node.ID)
	}
	for _, node := range console {
		ids = append(ids, node.ID)
	}
	return ids, nil
}

func (s *Service) roundRobinAssignAll(ctx context.Context, provider accountdomain.Provider, nodeIDs []uint64) (int, error) {
	if len(nodeIDs) == 0 {
		return 0, nil
	}
	accounts, err := s.accounts.ListEgressAssignments(ctx, provider)
	if err != nil {
		return 0, err
	}
	accountIDs := make([]uint64, 0, len(accounts))
	for _, credential := range accounts {
		accountIDs = append(accountIDs, credential.ID)
	}
	groups := roundRobinAccountGroups(accountIDs, nodeIDs)
	assigned := 0
	now := time.Now().UTC()
	for nodeID, ids := range groups {
		target := nodeID
		updated, updateErr := s.accounts.UpdateEgressBindings(ctx, provider, ids, &target, accountdomain.EgressAssignmentManual, now)
		if updateErr != nil {
			return assigned, updateErr
		}
		assigned += int(updated)
	}
	return assigned, nil
}

func roundRobinAccountGroups(accountIDs, nodeIDs []uint64) map[uint64][]uint64 {
	groups := make(map[uint64][]uint64, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return groups
	}
	for index, accountID := range accountIDs {
		nodeID := nodeIDs[index%len(nodeIDs)]
		groups[nodeID] = append(groups[nodeID], accountID)
	}
	return groups
}

func normalizeExternalProxyURLs(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("%w: 代理地址列表不能为空", ErrInvalidInput)
	}
	if len(values) > maxExternalProxyURLs {
		return nil, fmt.Errorf("%w: 代理地址最多 %d 条", ErrInvalidInput, maxExternalProxyURLs)
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			return nil, fmt.Errorf("%w: 代理地址不能为空", ErrInvalidInput)
		}
		parsed, err := NormalizeProxyURL(value)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		if parsed == "" {
			return nil, fmt.Errorf("%w: 代理地址不能为空", ErrInvalidInput)
		}
		if _, exists := seen[parsed]; exists {
			return nil, fmt.Errorf("%w: 代理地址重复", ErrInvalidInput)
		}
		seen[parsed] = struct{}{}
		normalized = append(normalized, parsed)
	}
	return normalized, nil
}
