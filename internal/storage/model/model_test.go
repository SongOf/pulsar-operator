package model

import (
	"encoding/json"
	"testing"
)

func TestSizeIsStrictAndBounded(t *testing.T) {
	for _, value := range []any{nil, true, "1", 1.0, json.Number("1.0"), json.Number("1e2"), json.Number("9223372036854775808"), 0, -1, 1048577} {
		if _, err := SizeGiB(value); err == nil {
			t.Errorf("accepted invalid size %#v", value)
		}
	}
	for _, value := range []any{1, int64(1), json.Number("1")} {
		if size, err := SizeGiB(value); err != nil || size != GiB {
			t.Errorf("%#v: %d %v", value, size, err)
		}
	}
	if size, err := SizeGiB(json.Number("1048576")); err != nil || size != 1048576*GiB {
		t.Fatalf("upper boundary: %d %v", size, err)
	}
}

func TestNamesAndIdentifiersRejectPathInjection(t *testing.T) {
	for _, value := range []any{nil, 12, "", "../a", "a/b", "a%2fb", "a b", "a?uid=x"} {
		if _, err := Identifier(value); err == nil {
			t.Errorf("unsafe identifier: %#v", value)
		}
	}
	if _, err := Identifier("ns~volume-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := NodeName("worker-0.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := Name("worker.example"); err == nil {
		t.Fatal("pool name accepted dot")
	}
}
