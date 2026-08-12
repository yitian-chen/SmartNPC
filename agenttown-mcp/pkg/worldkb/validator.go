package worldkb

// validator.go validates a merged KB for ID format and cross-reference
// consistency per docs/AgentTown_WorldKB_Design.md §8.5 and §9.6.
//
// Issues are classified by severity. Errors block YAML output; warnings are
// reported but do not block.

import (
	"regexp"
)

// IssueSeverity classifies validation findings.
type IssueSeverity int

const (
	SeverityWarning IssueSeverity = iota
	SeverityError
)

// Issue is a single validation finding.
type Issue struct {
	Severity IssueSeverity
	Code     string // e.g. "DUPLICATE_ZONE_ID", "AUTHORED_DANGLING_ID"
	Entity   string // entity ID or "" for global
	Message  string
}

// IssueSet is the result of Validate.
type IssueSet struct {
	Errors   []Issue
	Warnings []Issue
}

// HasErrors reports whether any Error-severity issue was found.
func (s *IssueSet) HasErrors() bool { return len(s.Errors) > 0 }

// All returns all issues, errors first then warnings.
func (s *IssueSet) All() []Issue {
	out := make([]Issue, 0, len(s.Errors)+len(s.Warnings))
	out = append(out, s.Errors...)
	out = append(out, s.Warnings...)
	return out
}

// idRegex matches stable semantic IDs per §5.1: lowercase letters/digits/
// underscores, 3-64 chars, leading letter. Hyphens are permitted for
// generated instance IDs that carry a numeric suffix (e.g. "charge-1"...
// "charge-6" for multi-instance smart object groups). Agent IDs like "H-01"
// are exempt (validated separately).
var idRegex = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,63}$`)

// agentIDRegex allows uppercase + hyphen for Agent IDs (e.g. "H-01", "H-02").
var agentIDRegex = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{1,63}$`)

// Validate checks a KB for internal consistency. It does NOT re-check
// merge-time concerns (duplicate IDs and dangling authored IDs are caught by
// Merge itself); instead it focuses on cross-references and ID format.
func Validate(kb *KB) *IssueSet {
	set := &IssueSet{}
	if kb == nil {
		set.Errors = append(set.Errors, Issue{Code: "NIL_KB", Message: "kb is nil"})
		return set
	}

	zoneIDs := make(map[string]bool, len(kb.Zones))
	for _, z := range kb.Zones {
		if !idRegex.MatchString(z.ID) {
			set.Errors = append(set.Errors, Issue{
				Code: "INVALID_ID_FORMAT", Entity: z.ID,
				Message: "zone id does not match ^[a-z][a-z0-9_-]{2,63}$",
			})
		}
		zoneIDs[z.ID] = true
	}

	// Object checks.
	for _, o := range kb.Objects {
		if !idRegex.MatchString(o.ID) {
			set.Errors = append(set.Errors, Issue{
				Code: "INVALID_ID_FORMAT", Entity: o.ID,
				Message: "object id does not match ^[a-z][a-z0-9_-]{2,63}$",
			})
		}
		if o.ZoneID != "" && !zoneIDs[o.ZoneID] {
			set.Errors = append(set.Errors, Issue{
				Code: "OBJECT_ZONE_REF_INVALID", Entity: o.ID,
				Message: "object references non-existent zone_id " + o.ZoneID,
			})
		}
		if o.Category == "" {
			set.Warnings = append(set.Warnings, Issue{
				Code: "EMPTY_OBJECT_CATEGORY", Entity: o.ID,
				Message: "object has empty category",
			})
		}
		if len(o.AvailableInteractions) == 0 {
			set.Warnings = append(set.Warnings, Issue{
				Code: "EMPTY_OBJECT_ACTIONS", Entity: o.ID,
				Message: "object has no available_interactions",
			})
		}
	}

	// Agent checks.
	agentIDs := make(map[string]bool, len(kb.Agents))
	for _, a := range kb.Agents {
		if !agentIDRegex.MatchString(a.ID) {
			set.Errors = append(set.Errors, Issue{
				Code: "INVALID_AGENT_ID_FORMAT", Entity: a.ID,
				Message: "agent id does not match ^[A-Za-z][A-Za-z0-9_-]{1,63}$",
			})
		}
		agentIDs[a.ID] = true
		if a.InitialZone != "" && !zoneIDs[a.InitialZone] {
			set.Errors = append(set.Errors, Issue{
				Code: "AGENT_INITIAL_ZONE_REF_INVALID", Entity: a.ID,
				Message: "agent references non-existent initial_zone " + a.InitialZone,
			})
		}
	}

	// Zone connections checks (NEW schema: structured Connection replaces
	// ConnectedTo []string). Validates that each Connection.To exists.
	for _, z := range kb.Zones {
		for _, conn := range z.Connections {
			if !zoneIDs[conn.To] {
				set.Errors = append(set.Errors, Issue{
					Code: "ZONE_CONNECTION_REF_INVALID", Entity: z.ID,
					Message: "zone connections references non-existent zone " + conn.To,
				})
			}
		}
	}

	// Relationship checks.
	for i, r := range kb.Relationships {
		if !agentIDs[r.From] {
			set.Errors = append(set.Errors, Issue{
				Code: "RELATIONSHIP_FROM_INVALID",
				Message: "relationship[" + itoa(i) + "].from references non-existent agent " + r.From,
			})
		}
		if !agentIDs[r.To] {
			set.Errors = append(set.Errors, Issue{
				Code: "RELATIONSHIP_TO_INVALID",
				Message: "relationship[" + itoa(i) + "].to references non-existent agent " + r.To,
			})
		}
	}

	return set
}

// itoa is a tiny strconv.Itoa to avoid importing strconv just for one call.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
