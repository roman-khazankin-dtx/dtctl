package exec

import (
	"testing"
)

func TestRawSmartscapeIDCompare(t *testing.T) {
	cases := []struct {
		query, field, lit string
	}{
		{`smartscapeEdges "*" | filter source_id == "SERVICE-00A909C21815A72E"`, "source_id", "SERVICE-00A909C21815A72E"},
		{`smartscapeEdges "*" | filter target_id=="PROCESS-6DF23DEAA911D4CE"`, "target_id", "PROCESS-6DF23DEAA911D4CE"},
		{`smartscapeNodes SERVICE | filter id != "SERVICE-00A909C21815A72E"`, "id", "SERVICE-00A909C21815A72E"},
		{`smartscapeNodes SERVICE | filter in(id, {"SERVICE-00A909C21815A72E", "SERVICE-00A909C21815A72F"})`, "id", "SERVICE-00A909C21815A72E"},
		{"  smartscapeNodes PROCESS_GROUP_INSTANCE\n| filter (id == \"PROCESS_GROUP_INSTANCE-6DF23DEAA911D4CE\")", "id", "PROCESS_GROUP_INSTANCE-6DF23DEAA911D4CE"},
		// wrapped already: fine
		{`smartscapeEdges "*" | filter source_id == toSmartscapeId("SERVICE-00A909C21815A72E")`, "", ""},
		// dt.process_group.id is a plain string on smartscape nodes, and compares fine
		{`smartscapeNodes PROCESS | filter dt.process_group.id == "PROCESS_GROUP-9CD97D4DE2909D73"`, "", ""},
		// not a smartscape query: dt.entity.* lookback ids are strings
		{`fetch dt.entity.service | filter id == "SERVICE-00A909C21815A72E"`, "", ""},
		// not an entity id
		{`smartscapeNodes SERVICE | filter id == "checkout"`, "", ""},
	}
	for _, c := range cases {
		f, l := RawSmartscapeIDCompare(c.query)
		if f != c.field || l != c.lit {
			t.Errorf("%s:\n got (%q, %q), want (%q, %q)", c.query, f, l, c.field, c.lit)
		}
	}
}

// The case from an agent trace: a raw SERVICE id on smartscapeEdges returned 0
// rows and the advice said to widen the window, which the agent did, in vain.
func TestEmptyResultAdvice_RawSmartscapeIDReplacesWindowAdvice(t *testing.T) {
	p := &fakeProbe{respond: func(string) (*DQLQueryResponse, error) { return recordsResponse(), nil }}
	e := &DQLExecutor{probe: p.run}
	query := `smartscapeEdges "*" | filter source_id == "SERVICE-00A909C21815A72E" | limit 5`

	reason, sugg := e.emptyResultAdvice(query, recordsResponse(), nil, DQLExecuteOptions{})

	if reason == nil || reason.Code != "raw_smartscape_id" || reason.Field != "source_id" {
		t.Fatalf("reason = %+v, suggestions = %v", reason, sugg)
	}
	if !hasSuggestion(sugg, `toSmartscapeId("SERVICE-00A909C21815A72E")`) {
		t.Errorf("suggestions should give the fix: %v", sugg)
	}
	if hasSuggestion(sugg, "widen") || hasSuggestion(sugg, "DEFAULT query window") {
		t.Errorf("widen-the-window advice must be withheld: %v", sugg)
	}
	if len(p.calls) != 0 {
		t.Errorf("the finding is decided from the query alone, probes = %q", p.calls)
	}
}

func TestEmptyFromRawSmartscapeID(t *testing.T) {
	q := `smartscapeNodes SERVICE | filter id == "SERVICE-00A909C21815A72E" | summarize c = count()`
	if !EmptyFromRawSmartscapeID(q, []map[string]interface{}{}) {
		t.Error("no rows: want true")
	}
	if !EmptyFromRawSmartscapeID(q, []map[string]interface{}{{"c": float64(0)}}) {
		t.Error("all-zero count row: want true")
	}
	if EmptyFromRawSmartscapeID(q, []map[string]interface{}{{"c": float64(1)}}) {
		t.Error("a match: want false")
	}
	if EmptyFromRawSmartscapeID(q, nil) {
		t.Error("streamed to disk (nil records): want false")
	}
}
