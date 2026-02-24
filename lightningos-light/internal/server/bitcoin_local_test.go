package server

import "testing"

func TestRPCAllowListContainsIPCoveredByCIDR(t *testing.T) {
	lines := []string{
		"rpcallowip=127.0.0.1",
		"rpcallowip=172.21.0.0/16",
	}
	if !rpcAllowListContains(lines, "172.21.0.42") {
		t.Fatalf("expected IP to be allowed by CIDR")
	}
}

func TestRPCAllowListContainsCIDRExactMatch(t *testing.T) {
	lines := []string{
		"rpcallowip=172.22.0.0/16",
	}
	if !rpcAllowListContains(lines, "172.22.0.0/16") {
		t.Fatalf("expected CIDR exact match to be detected")
	}
}

func TestEnsureBitcoinCoreRPCAllowListAvoidsDuplicateCIDR(t *testing.T) {
	raw := "rpcallowip=127.0.0.1\nrpcallowip=172.23.0.0/16\n"
	updated, changed := ensureBitcoinCoreRPCAllowList(raw, []string{"172.23.0.0/16"})
	if changed {
		t.Fatalf("expected no change when CIDR already exists, got: %q", updated)
	}
}
