// Package profile loads NPC persona documents (profile.md) from a directory.
//
// Each file is named <agentID>.md (e.g. H-01.md) and uses fixed H2 section
// headers to declare the five persona fields consumed by prompt.AgentRole:
//
//	## 名字
//	## 职业
//	## 背景
//	## 性格特质
//	## 说话风格
//	## 属性分段
//
// The optional ## 属性分段 section declares per-attribute band boundaries
// (e.g. "疲劳: 40,70,90") used to render physical state as range labels
// instead of raw numbers, with per-NPC thresholds.
//
// Unknown sections are ignored. The parsed Profile is merged with the
// world_kb Agent entry and the hardcoded fallback by prompt.AgentRole
// (profile non-empty > KB non-empty > fallback non-empty).
package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Profile corresponds to the five output fields of prompt.AgentRole.
// Empty values mean the field is undeclared in the profile.md; AgentRole
// falls back to KB / hardcoded fallback for those fields.
type Profile struct {
	AgentID     string   // derived from filename (without .md), not markdown body
	DisplayName string   // ## 名字
	Profession  string   // ## 职业
	Description string   // ## 背景
	Traits      []string // ## 性格特质 (split by 、 or newline)
	SpeechStyle string   // ## 说话风格
	// AttrBands holds per-attribute band boundaries from ## 属性分段, keyed
	// by attribute name (电量/疲劳/关节磨损; 旧名"能量"兼容读取). Each value
	// is 3 ascending thresholds splitting the 0-100 range into 4 bands.
	// Only attributes explicitly listed in the profile are present; consumers
	// fall back to global defaults for the rest. nil when the section is absent.
	AttrBands map[string][3]float64
}

// Section titles recognized in profile.md. Unknown `## xxx` sections are
// dropped during parsing.
const (
	sectionDisplayName = "名字"
	sectionProfession  = "职业"
	sectionDescription = "背景"
	sectionTraits      = "性格特质"
	sectionSpeechStyle = "说话风格"
	sectionAttrBands   = "属性分段"
)

// bandAttrNames are the attribute names allowed in ## 属性分段.
// 2026-08-24 物理属性名"能量"改称"电量"；保留"能量"旧名兼容尚未迁移的 profile.md。
var bandAttrNames = map[string]struct{}{
	"电量":   {},
	"能量":   {},
	"疲劳":   {},
	"关节磨损": {},
}

// LoadDir scans dir for *.md files and maps agentID (filename without
// extension) → *Profile. Empty dir returns an empty map + nil error.
// Non-.md files are skipped. Malformed files (no headers) yield an empty
// Profile rather than an error so callers can still override via filename.
func LoadDir(dir string) (map[string]*Profile, error) {
	out := make(map[string]*Profile)
	if dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read profiles dir %q: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		agentID := strings.TrimSuffix(e.Name(), ".md")
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read profile %q: %w", e.Name(), err)
		}
		sections := parseMarkdown(string(raw))
		p := &Profile{AgentID: agentID}
		p.DisplayName = strings.TrimSpace(sections[sectionDisplayName])
		p.Profession = strings.TrimSpace(sections[sectionProfession])
		p.Description = strings.TrimSpace(sections[sectionDescription])
		p.Traits = parseTraits(sections[sectionTraits])
		p.SpeechStyle = strings.TrimSpace(sections[sectionSpeechStyle])
		p.AttrBands = parseAttrBands(sections[sectionAttrBands])
		out[agentID] = p
	}
	return out, nil
}

// parseMarkdown scans content line-by-line, splitting on `## title` headers.
// Returns a map of title → body (body is the raw text between this header
// and the next header, with the trailing newline trimmed). Unknown titles
// are kept in the map so callers can decide what to consume; LoadDir only
// reads the 5 known sections.
func parseMarkdown(content string) map[string]string {
	out := make(map[string]string)
	var curTitle string
	var body strings.Builder
	flush := func() {
		if curTitle != "" {
			out[curTitle] = body.String()
		}
		body.Reset()
	}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, "## ") {
			flush()
			curTitle = strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			continue
		}
		if curTitle != "" {
			if body.Len() > 0 {
				body.WriteString("\n")
			}
			body.WriteString(trimmed)
		}
	}
	flush()
	return out
}

// parseAttrBands parses the ## 属性分段 body into per-attribute band
// boundaries. Each line is `属性名: t1,t2,t3` (3 ascending thresholds).
// Lines with unknown attribute names, wrong value counts, non-numeric
// values, or non-ascending thresholds are skipped (consistent with the
// package's tolerant parsing style). Returns nil when no valid line exists.
func parseAttrBands(body string) map[string][3]float64 {
	var out map[string][3]float64
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			name, rest, ok = strings.Cut(line, "：")
		}
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if _, known := bandAttrNames[name]; !known {
			continue
		}
		parts := strings.Split(rest, ",")
		if len(parts) != 3 {
			continue
		}
		var th [3]float64
		valid := true
		for i, p := range parts {
			v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
			if err != nil {
				valid = false
				break
			}
			th[i] = v
		}
		if !valid || !(th[0] < th[1] && th[1] < th[2]) {
			continue
		}
		if out == nil {
			out = make(map[string][3]float64)
		}
		out[name] = th
	}
	return out
}

// parseTraits splits the ## 性格特质 body into []string. Supports two
// formats: single line separated by 、 (e.g. "沉稳、念旧、务实"), or one
// trait per line. Empty entries are dropped.
func parseTraits(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, part := range strings.Split(line, "、") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}
