package grpcspec

import "testing"

func TestReadVerbNeedsWordBoundary(t *testing.T) {
	for name, want := range map[string]bool{
		"GetUser": true, "Get": true, "ListOrders": true, "Count": true,
		"CheckoutCart": false, "CountryUpdate": false, "StreamlineOrder": false, "Searchable": false,
	} {
		if got := readVerb.MatchString(name); got != want {
			t.Errorf("readVerb(%q) = %v, want %v", name, got, want)
		}
	}
}
