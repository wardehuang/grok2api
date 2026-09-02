package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestReplaceWebConsoleProxiesLeavesBuildAndAssetsAndRoundRobins(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)

	buildNode := createHealthyEgressNodeForScope(t, ctx, nodes, cipher, "keep-build", egress.ScopeBuild, 0)
	oldWeb := createHealthyEgressNodeForScope(t, ctx, nodes, cipher, "old-web", egress.ScopeWeb, 0)
	oldConsole := createHealthyEgressNodeForScope(t, ctx, nodes, cipher, "old-console", egress.ScopeConsole, 0)
	asset := createHealthyEgressNodeForScope(t, ctx, nodes, cipher, "keep-web-asset", egress.ScopeWebAsset, 0)

	webAccounts := []account.Credential{
		createEgressOperationsProviderAccount(t, ctx, accounts, account.ProviderWeb, "web-a"),
		createEgressOperationsProviderAccount(t, ctx, accounts, account.ProviderWeb, "web-b"),
		createEgressOperationsProviderAccount(t, ctx, accounts, account.ProviderWeb, "web-c"),
	}
	consoleAccounts := []account.Credential{
		createEgressOperationsProviderAccount(t, ctx, accounts, account.ProviderConsole, "console-a"),
		createEgressOperationsProviderAccount(t, ctx, accounts, account.ProviderConsole, "console-b"),
	}
	buildAccount := createEgressOperationsProviderAccount(t, ctx, accounts, account.ProviderBuild, "build-a")
	now := time.Now().UTC()
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderWeb, []uint64{webAccounts[0].ID}, &oldWeb.ID, account.EgressAssignmentManual, now); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, []uint64{buildAccount.ID}, &buildNode.ID, account.EgressAssignmentManual, now); err != nil {
		t.Fatal(err)
	}

	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	result, err := service.ReplaceWebConsoleProxies(ctx, []string{
		"socks5://user:pass@10.0.0.1:1080",
		"http://10.0.0.2:8080",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 2 || result.Created != 4 || result.WebAssigned != 3 || result.ConsoleAssigned != 2 {
		t.Fatalf("result = %#v", result)
	}
	if len(result.WebNodes) != 2 || len(result.ConsoleNodes) != 2 {
		t.Fatalf("nodes = web %#v console %#v", result.WebNodes, result.ConsoleNodes)
	}

	listed, err := nodes.ListEgressNodes(ctx, "", repository.SortQuery{})
	if err != nil {
		t.Fatal(err)
	}
	remaining := map[uint64]egress.Scope{}
	for _, node := range listed {
		remaining[node.ID] = node.Scope
	}
	if remaining[buildNode.ID] != egress.ScopeBuild || remaining[asset.ID] != egress.ScopeWebAsset {
		t.Fatalf("preserved nodes missing: %#v", remaining)
	}
	if _, ok := remaining[oldWeb.ID]; ok {
		t.Fatal("old web node was not deleted")
	}
	if _, ok := remaining[oldConsole.ID]; ok {
		t.Fatal("old console node was not deleted")
	}

	webLoads := map[uint64]int{}
	for _, credential := range webAccounts {
		actual, getErr := accounts.Get(ctx, credential.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if actual.EgressAssignmentMode != account.EgressAssignmentManual {
			t.Fatalf("web account %d mode = %q", actual.ID, actual.EgressAssignmentMode)
		}
		if actual.EgressNodeID != result.WebNodes[0].ID && actual.EgressNodeID != result.WebNodes[1].ID {
			t.Fatalf("web account %d node = %d", actual.ID, actual.EgressNodeID)
		}
		webLoads[actual.EgressNodeID]++
	}
	if webLoads[result.WebNodes[0].ID] != 2 || webLoads[result.WebNodes[1].ID] != 1 {
		t.Fatalf("web loads = %#v", webLoads)
	}

	consoleLoads := map[uint64]int{}
	for _, credential := range consoleAccounts {
		actual, getErr := accounts.Get(ctx, credential.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if actual.EgressAssignmentMode != account.EgressAssignmentManual {
			t.Fatalf("console account %d mode = %q", actual.ID, actual.EgressAssignmentMode)
		}
		consoleLoads[actual.EgressNodeID]++
	}
	if consoleLoads[result.ConsoleNodes[0].ID] != 1 || consoleLoads[result.ConsoleNodes[1].ID] != 1 {
		t.Fatalf("console loads = %#v", consoleLoads)
	}

	keptBuild, err := accounts.Get(ctx, buildAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if keptBuild.EgressNodeID != buildNode.ID || keptBuild.EgressAssignmentMode != account.EgressAssignmentManual {
		t.Fatalf("build binding = %#v", keptBuild)
	}

	firstProxy, err := service.ProxyURL(ctx, result.WebNodes[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstProxy != "socks5://user:pass@10.0.0.1:1080" {
		t.Fatalf("web proxy = %q", firstProxy)
	}
}

func TestReplaceWebConsoleProxiesRejectsInvalidURLWithoutDeleting(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	oldWeb := createHealthyEgressNodeForScope(t, ctx, nodes, cipher, "keep-on-error", egress.ScopeWeb, 0)

	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	_, err := service.ReplaceWebConsoleProxies(ctx, []string{"not-a-proxy"})
	if !errors.Is(err, egressapp.ErrInvalidInput) {
		t.Fatalf("err = %v", err)
	}
	if _, getErr := nodes.GetEgressNode(ctx, oldWeb.ID); getErr != nil {
		t.Fatalf("old node missing after invalid replace: %v", getErr)
	}
}
