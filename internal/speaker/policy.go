package speaker

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/server"
)

const (
	setIPv4      = "announced-ipv4"
	setIPv6      = "announced-ipv6"
	policyImport = "import-local-only"
	policyExport = "export-announced-only"
)

func accept(name string, conditions *api.Conditions) *api.Statement {
	return &api.Statement{
		Name:       name,
		Conditions: conditions,
		Actions:    &api.Actions{RouteAction: api.RouteAction_ROUTE_ACTION_ACCEPT},
	}
}

func announcedStatement(set string) *api.Statement {
	return accept("export-"+set, &api.Conditions{
		RouteType: api.Conditions_ROUTE_TYPE_LOCAL,
		PrefixSet: &api.MatchSet{Type: api.MatchSet_TYPE_ANY, Name: set},
	})
}

func prefixSet(name string, prefixes ...netip.Prefix) *api.DefinedSet {
	set := &api.DefinedSet{DefinedType: api.DefinedType_DEFINED_TYPE_PREFIX, Name: name}
	for _, prefix := range prefixes {
		bits := uint32(prefix.Bits())
		set.Prefixes = append(set.Prefixes, &api.Prefix{IpPrefix: prefix.String(), MaskLengthMin: bits, MaskLengthMax: bits})
	}

	return set
}

func setName(prefix netip.Prefix) string {
	if prefix.Addr().Is4() {
		return setIPv4
	}

	return setIPv6
}

func installPolicies(ctx context.Context, bgp *server.BgpServer) error {
	importPolicy := &api.Policy{Name: policyImport, Statements: []*api.Statement{
		accept("import-local", &api.Conditions{RouteType: api.Conditions_ROUTE_TYPE_LOCAL}),
	}}
	exportPolicy := &api.Policy{Name: policyExport, Statements: []*api.Statement{
		announcedStatement(setIPv4),
		announcedStatement(setIPv6),
	}}
	err := bgp.SetPolicies(ctx, &api.SetPoliciesRequest{
		DefinedSets: []*api.DefinedSet{prefixSet(setIPv4), prefixSet(setIPv6)},
		Policies:    []*api.Policy{importPolicy, exportPolicy},
	})
	if err != nil {
		return fmt.Errorf("install policies: %w", err)
	}

	assignments := []*api.PolicyAssignment{
		assignment(api.PolicyDirection_POLICY_DIRECTION_IMPORT, policyImport),
		assignment(api.PolicyDirection_POLICY_DIRECTION_EXPORT, policyExport),
	}
	for _, item := range assignments {
		if err := bgp.SetPolicyAssignment(ctx, &api.SetPolicyAssignmentRequest{Assignment: item}); err != nil {
			return fmt.Errorf("assign policies: %w", err)
		}
	}

	return nil
}

func assignment(direction api.PolicyDirection, policy string) *api.PolicyAssignment {
	return &api.PolicyAssignment{
		Name:          "global",
		Direction:     direction,
		Policies:      []*api.Policy{{Name: policy}},
		DefaultAction: api.RouteAction_ROUTE_ACTION_REJECT,
	}
}

func allowPrefix(ctx context.Context, bgp *server.BgpServer, prefix netip.Prefix) error {
	return bgp.AddDefinedSet(ctx, &api.AddDefinedSetRequest{DefinedSet: prefixSet(setName(prefix), prefix)})
}

func denyPrefix(ctx context.Context, bgp *server.BgpServer, prefix netip.Prefix) error {
	return bgp.DeleteDefinedSet(ctx, &api.DeleteDefinedSetRequest{DefinedSet: prefixSet(setName(prefix), prefix)})
}
