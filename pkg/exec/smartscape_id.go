package exec

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Dynatrace/dtctl/pkg/output"
)

// Raw smartscape ids on an empty result.
//
// On smartscapeNodes and smartscapeEdges, id, source_id and target_id are typed
// smartscape ids, not strings. A filter that compares one with a string literal,
// `filter source_id == "SERVICE-00A909C21815A72E"`, succeeds with zero rows, and
// the only advice an agent got was "widen the window", which can never help: the
// literal needs toSmartscapeId("SERVICE-..."). Other id-looking fields, such as
// dt.process_group.id, are plain strings and compare fine, so only these three are
// matched. Seen in an agent trace on 2026-11-16 (service -> processes via edges).

// rawSmartscapeIDRe matches `<field> == "TYPE-HEX"` / `!=`, and the first literal
// of `in(<field>, ...)`. The field must not follow a dot or a word character, so
// dt.process_group.id is not taken for id.
var rawSmartscapeIDRe = regexp.MustCompile(
	`(?:^|[^\w.])(id|source_id|target_id)\s*(?:==|!=)\s*"([A-Z][A-Z0-9_]*-[0-9A-F]{16})"` +
		`|\bin\s*\(\s*(id|source_id|target_id)\s*,\s*\{?\s*"([A-Z][A-Z0-9_]*-[0-9A-F]{16})"`)

// smartscapeCommandRe matches a query whose first command reads smartscape.
var smartscapeCommandRe = regexp.MustCompile(`^\s*smartscape(?:Nodes|Edges)\b`)

// RawSmartscapeIDCompare reports the first id field of a smartscape query that is
// compared with a raw string literal, and that literal. Empty when there is none.
func RawSmartscapeIDCompare(query string) (field, literal string) {
	if !smartscapeCommandRe.MatchString(query) {
		return "", ""
	}
	m := rawSmartscapeIDRe.FindStringSubmatch(query)
	switch {
	case m == nil:
		return "", ""
	case m[1] != "":
		return m[1], m[2]
	default:
		return m[3], m[4]
	}
}

// EmptyFromRawSmartscapeID reports whether records is an empty result (no rows, or
// the all-zero row of a summarize count()) of a smartscape query that compares an
// id with a raw string literal.
func EmptyFromRawSmartscapeID(query string, records []map[string]interface{}) bool {
	if f, _ := RawSmartscapeIDCompare(query); f == "" {
		return false
	}
	return records != nil && isEmptyResult(records)
}

// rawSmartscapeIDAdvice explains an empty smartscape result caused by a raw id
// literal. It replaces the widen-the-window advice, which cannot help here.
func rawSmartscapeIDAdvice(query string) (*output.EmptyReason, []string) {
	field, lit := RawSmartscapeIDCompare(query)
	if field == "" {
		return nil, nil
	}
	reason := &output.EmptyReason{
		Code:     "raw_smartscape_id",
		Field:    field,
		Evidence: fmt.Sprintf("`%s` is a smartscape id and the query compares it with the string %q, which matches no row in any window", field, lit),
	}
	fix := fmt.Sprintf("%s == toSmartscapeId(%q)", field, lit)
	if strings.Contains(query, "in(") || strings.Contains(query, "in (") {
		fix += " (in an in() list, wrap each literal)"
	}
	return reason, []string{fmt.Sprintf("# `%s` is a smartscape id, not a string, so comparing it with \"%s\" matches nothing whatever the window — wrap the literal: `%s`", field, lit, fix)}
}
