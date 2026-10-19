package main

import (
	"slices"
	"testing"
)

func TestParseOnly(t *testing.T) {
	if got := parseOnly(" images, cicd,,"); !slices.Equal(got, []string{"images", "cicd"}) {
		t.Errorf("got %q", got)
	}
	if got := parseOnly(""); got != nil {
		t.Errorf("empty --only = %q, want nil (apply everything)", got)
	}
}
