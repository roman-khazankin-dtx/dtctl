package codelevelanalysis

import (
	"net/url"
	"strings"
	"testing"
)

func TestEncodeLeafType(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{LeafTypeTotal, "", false},
		{LeafTypeBackground, "42\x110", false},
		{"ambient", "42\x110", false},
		{LeafTypeService, "42\x111", false},
		{"bogus", "", true},
	}
	for _, tc := range cases {
		got, err := encodeLeafType(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("encodeLeafType(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("encodeLeafType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLeafTypeWireEncoding pins the exact servicefilter query value the server
// expects: background -> 42%110, service -> 42%111, total -> omitted.
func TestLeafTypeWireEncoding(t *testing.T) {
	cases := []struct {
		leafType     string
		wantContains string // "" means servicefilter must be absent
	}{
		{LeafTypeTotal, ""},
		{LeafTypeBackground, "servicefilter=42%110"},
		{LeafTypeService, "servicefilter=42%111"},
	}
	for _, tc := range cases {
		p := Payload{Kind: "methodHotspots", EntityID: "PROCESS_GROUP_INSTANCE-ABC", From: 1, To: 2, LeafType: tc.leafType}
		path := buildSubmitPath(p)
		if tc.wantContains == "" {
			if strings.Contains(path, "servicefilter") {
				t.Errorf("leafType=%q: expected no servicefilter, got %q", tc.leafType, path)
			}
			continue
		}
		if !strings.Contains(path, tc.wantContains) {
			t.Errorf("leafType=%q: path %q does not contain %q", tc.leafType, path, tc.wantContains)
		}
	}
}

func TestValidateEntityType(t *testing.T) {
	base := Payload{Kind: "methodHotspots", From: 1, To: 2}
	cases := []struct {
		entity  string
		wantErr bool
	}{
		{"PROCESS_GROUP-ABC", false},
		{"PROCESS_GROUP_INSTANCE-ABC", false},
		{"SERVICE-ABC", true},
		{"HOST-ABC", true},
		{"", true},
	}
	for _, tc := range cases {
		p := base
		p.EntityID = tc.entity
		err := validate(p)
		if (err != nil) != tc.wantErr {
			t.Errorf("validate(entity=%q) err = %v, wantErr %v", tc.entity, err, tc.wantErr)
		}
	}
}

func TestValidateRejectsBadLeafType(t *testing.T) {
	p := Payload{Kind: "methodHotspots", EntityID: "PROCESS_GROUP-ABC", From: 1, To: 2, LeafType: "nope"}
	if err := validate(p); err == nil {
		t.Fatalf("expected validate to reject unknown leaf type")
	}
}

// TestServiceFilterEscapeHatch confirms a raw ServiceFilter is used verbatim
// when LeafType is unset, and that LeafType wins when both are set.
func TestServiceFilterEscapeHatch(t *testing.T) {
	raw := Payload{Kind: "methodHotspots", EntityID: "PROCESS_GROUP-ABC", From: 1, To: 2, ServiceFilter: "42\x110"}
	if got := buildSubmitPath(raw); !strings.Contains(got, "servicefilter=42%110") {
		t.Errorf("raw ServiceFilter not honored: %q", got)
	}
	both := Payload{Kind: "methodHotspots", EntityID: "PROCESS_GROUP-ABC", From: 1, To: 2, ServiceFilter: "42\x110", LeafType: LeafTypeService}
	if got := buildSubmitPath(both); !strings.Contains(got, "servicefilter=42%111") {
		t.Errorf("LeafType should win over raw ServiceFilter: %q", got)
	}
}

// guard against url encoding drift.
func TestDC1EscapesToPercent11(t *testing.T) {
	v := url.Values{}
	v.Set("servicefilter", "42\x110")
	if got := v.Encode(); got != "servicefilter=42%110" {
		t.Fatalf("DC1 escape drifted: %q", got)
	}
}
