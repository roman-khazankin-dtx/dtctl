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
		{"PROCESS-ABC", false}, // 3rd-gen PGI rename
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

// TestNormalizeEntityID pins the 3rd-gen PROCESS- -> classic
// PROCESS_GROUP_INSTANCE- rewrite, and that other prefixes pass through.
func TestNormalizeEntityID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"PROCESS-A89D22C1D0B350C4", "PROCESS_GROUP_INSTANCE-A89D22C1D0B350C4"},
		{"PROCESS_GROUP_INSTANCE-ABC", "PROCESS_GROUP_INSTANCE-ABC"}, // already classic
		{"PROCESS_GROUP-ABC", "PROCESS_GROUP-ABC"},                   // group, not instance
		{"SERVICE-ABC", "SERVICE-ABC"},                              // untouched
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeEntityID(tc.in); got != tc.want {
			t.Errorf("normalizeEntityID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSubmitPathRewritesProcessEntity confirms the outgoing request path carries
// the classic PGI id, never the 3rd-gen PROCESS- id the API 404s on.
func TestSubmitPathRewritesProcessEntity(t *testing.T) {
	p := Payload{Kind: "methodHotspots", EntityID: "PROCESS-A89D22C1D0B350C4", From: 1, To: 2}
	path := buildSubmitPath(p)
	if !strings.Contains(path, "PROCESS_GROUP_INSTANCE-A89D22C1D0B350C4") {
		t.Errorf("path %q does not carry the classic PGI id", path)
	}
	if strings.Contains(path, "/PROCESS-A89D22C1D0B350C4?") {
		t.Errorf("path %q still carries the 3rd-gen PROCESS- id", path)
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
