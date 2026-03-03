package server

import "testing"

func TestIsManagedRebalanceMemo(t *testing.T) {
	tests := []struct {
		name string
		memo string
		want bool
	}{
		{name: "valid memo", memo: "rebalance:42:123:456", want: true},
		{name: "valid memo uppercase", memo: "Rebalance:1:2:3", want: true},
		{name: "invalid prefix", memo: "rebalance attempt", want: false},
		{name: "missing parts", memo: "rebalance:1:2", want: false},
		{name: "non numeric ids", memo: "rebalance:a:b:c", want: false},
		{name: "empty", memo: "", want: false},
	}

	for _, tc := range tests {
		if got := isManagedRebalanceMemo(tc.memo); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestShouldSuppressExternalFailedRebalance(t *testing.T) {
	tests := []struct {
		name   string
		status string
		memo   string
		want   bool
	}{
		{name: "succeeded external", status: "SUCCEEDED", memo: "", want: false},
		{name: "failed external", status: "FAILED", memo: "", want: true},
		{name: "failed managed", status: "FAILED", memo: "rebalance:12:34:56", want: false},
		{name: "in flight external", status: "IN_FLIGHT", memo: "", want: true},
	}

	for _, tc := range tests {
		if got := shouldSuppressExternalFailedRebalance(tc.status, tc.memo); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
