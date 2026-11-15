package domain

import (
	"fmt"
	"sort"
)

type Action string

const (
	Advertise Action = "advertise"
	Withdraw  Action = "withdraw"
)

type DesiredRoute struct {
	Key      string
	Endpoint Endpoint
}

type OwnedRoute struct {
	Key      string
	Endpoint Endpoint
	PathID   string
}

type Operation struct {
	Action Action
	Key    string
	PathID string
}

// Plan returns a deterministic withdrawal-first delta against confirmed
// receipts. It does not mutate state or perform external operations.
func Plan(desired []DesiredRoute, applied []OwnedRoute, foreign map[string]bool) ([]Operation, error) {
	wanted := make(map[string]Endpoint, len(desired))
	for _, route := range desired {
		if !route.Endpoint.Advertise {
			continue
		}
		if other, ok := wanted[route.Key]; ok && !other.Equivalent(route.Endpoint) {
			return nil, fmt.Errorf("conflicting desired owners or attributes for %s", route.Key)
		}
		if foreign[route.Key] {
			return nil, fmt.Errorf("unclaimed local path for %s", route.Key)
		}
		wanted[route.Key] = cloneEndpoint(route.Endpoint)
	}

	current := make(map[string]OwnedRoute, len(applied))
	for _, route := range applied {
		current[route.Key] = route
	}

	operations := make([]Operation, 0, len(wanted)+len(current))
	keys := make([]string, 0, len(current))
	for key := range current {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		owned := current[key]
		if endpoint, ok := wanted[key]; ok && owned.Endpoint.Equivalent(endpoint) {
			continue
		}
		operations = append(operations, Operation{Action: Withdraw, Key: key, PathID: owned.PathID})
	}

	keys = keys[:0]
	for key := range wanted {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		endpoint := wanted[key]
		if owned, ok := current[key]; ok && owned.Endpoint.Equivalent(endpoint) {
			continue
		}
		operations = append(operations, Operation{Action: Advertise, Key: key})
	}
	return operations, nil
}

func cloneEndpoint(endpoint Endpoint) Endpoint {
	endpoint.IPAddresses = append([]string(nil), endpoint.IPAddresses...)
	endpoint.RouteTargets = append([]string(nil), endpoint.RouteTargets...)
	return endpoint
}
