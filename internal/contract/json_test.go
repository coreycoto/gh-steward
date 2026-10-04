package contract

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestStrictMachineInputs(t *testing.T) {
	for _, data := range []string{`{"id":1,"id":2}`, `{"nested":{"x":1,"x":2}}`, `{"x":NaN}`, `{"x":Infinity}`, `{} {}`, `[]`, `null`, `{"x":`, strings.Repeat(`{"x":`, 70) + `0` + strings.Repeat(`}`, 70)} {
		t.Run(data[:min(len(data), 32)], func(t *testing.T) {
			if _, err := Decode(strings.NewReader(data)); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	o, err := Decode(strings.NewReader(`{"id":9007199254740993,"order":7.25,"empty":"","null":null,"items":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	n, err := PositiveInteger(o["id"])
	if err != nil || n != 9007199254740993 {
		t.Fatalf("integer lost precision: %v %v", n, err)
	}
	order, err := Number(o["order"])
	if err != nil || order != 7.25 {
		t.Fatalf("fractional order lost: %v %v", order, err)
	}
	if o["empty"] != "" || o["null"] != nil {
		t.Fatal("nullable values changed")
	}
	if _, err := Decode(strings.NewReader(`{"x":"` + strings.Repeat("x", MaxJSONBytes) + `"}`)); err == nil {
		t.Fatal("oversized input accepted")
	}
}

func TestIdentityNumbersRejectCoercion(t *testing.T) {
	for _, value := range []any{true, false, "25", json.Number("2.0"), json.Number("2.5"), json.Number("2e1"), json.Number("0"), json.Number("-1"), json.Number("9223372036854775808"), float64(25)} {
		if _, err := PositiveInteger(value); err == nil {
			t.Fatalf("identity accepted coercion for %v", value)
		}
	}
	for _, value := range []any{math.NaN(), math.Inf(1), true, "7", json.Number("1e999")} {
		if _, err := Number(value); err == nil {
			t.Fatal("invalid numeric field accepted")
		}
	}
}

func TestRepositoryIdentity(t *testing.T) {
	r, err := ParseRepository(Object{"nameWithOwner": "Example/Widgets", "url": "https://GitHub.com/Example/Widgets"})
	if err != nil || r.FullName() != "example/widgets" || r.Host != "github.com" {
		t.Fatalf("bad identity: %#v %v", r, err)
	}
	for _, u := range []string{"http://github.com/example/widgets", "https://other.example/example/other", "https://user:secret@github.com/example/widgets", "https://github.com:8443/example/widgets", "https://github.com/example/widgets?x=1", "https://github.com/example/widgets#x", "https://github.com/example/%77idgets"} {
		if _, err := ParseRepository(Object{"nameWithOwner": "example/widgets", "url": u}); err == nil {
			t.Fatalf("invalid target accepted: %s", u)
		}
	}
	if _, err := ParseRepository(Object{"nameWithOwner": "example/widgets", "url": r.URL, "host": "other.example"}); err == nil {
		t.Fatal("host conflict accepted")
	}
	for _, kind := range []string{"User", "Organization"} {
		provider := r.Object()
		provider["owner"] = Object{"login": "Example", "__typename": kind}
		if actual, err := ParseRepository(provider); err != nil || actual != r {
			t.Fatal("qualified native owner did not normalize to the exact repository", actual, err)
		}
	}
	for _, owner := range []Object{{"login": "other", "__typename": "User"}, {"login": false, "__typename": "User"}, {"login": "example", "__typename": "Bot"}} {
		provider := r.Object()
		provider["owner"] = owner
		if _, err := ParseRepository(provider); err == nil {
			t.Fatal("malformed or conflicting native owner accepted", owner)
		}
	}
	for _, key := range []string{"nameWithOwner", "host", "owner", "name"} {
		for _, value := range []any{false, int64(17), nil, Object{"login": "example"}} {
			object := r.Object()
			object[key] = value
			if _, err := ParseRepository(object); err == nil {
				t.Fatal("malformed redundant identity claim was ignored", key, value)
			}
		}
	}
}

func TestCanonicalClonesAndFiniteDigests(t *testing.T) {
	a := Object{"z": "<λ>", "a": Object{"x": []any{json.Number("7.25"), nil, true}}}
	b, err := Clone(a)
	if err != nil {
		t.Fatal(err)
	}
	b["z"] = "changed"
	if a["z"] != "<λ>" {
		t.Fatal("clone aliases input")
	}
	h, err := Digest(a)
	if err != nil || len(h) != 64 {
		t.Fatal("missing digest")
	}
	other := Object{"a": a["a"], "z": "<λ>"}
	same, _ := Digest(other)
	if h != same {
		t.Fatal("map ordering changed digest")
	}
	if _, err := Digest(Object{"x": math.NaN()}); err == nil {
		t.Fatal("nonfinite evidence accepted")
	}
}
