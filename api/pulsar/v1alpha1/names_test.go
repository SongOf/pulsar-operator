package v1alpha1

import (
	"strings"
	"testing"
)

func TestClusterNameBoundaries(t *testing.T) {
	for _, name := range []string{"a", "pulsar-01", strings.Repeat("a", 47)} {
		if err := ValidateClusterName(name); err != nil {
			t.Errorf("valid name %q: %v", name, err)
		}
	}
	for _, name := range []string{"", strings.Repeat("a", 48), strings.Repeat("a", 60), "1cluster", "a.b", "a-", "-a", "A"} {
		if err := ValidateClusterName(name); err == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
}

func TestBookKeeperNameBoundary(t *testing.T) {
	if err := ValidateBookKeeperName(strings.Repeat("a", 51)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBookKeeperName(strings.Repeat("a", 52)); err == nil {
		t.Fatal("accepted a name that exceeds the metadata Job limit")
	}
}
