package profile

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func writeProfile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadDir_NormalParse(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-01.md", "## 名字\n老陈\n\n## 职业\nsupervisor、worker\n\n## 背景\n车间主管\n\n## 性格特质\n沉稳、念旧、务实\n\n## 说话风格\n话少稳重\n")
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("got %d profiles, want 1", len(profiles))
	}
	p := profiles["H-01"]
	if p == nil {
		t.Fatal("H-01 missing")
	}
	if p.AgentID != "H-01" {
		t.Errorf("AgentID=%q want H-01", p.AgentID)
	}
	if p.DisplayName != "老陈" {
		t.Errorf("DisplayName=%q want 老陈", p.DisplayName)
	}
	if p.Profession != "supervisor、worker" {
		t.Errorf("Profession=%q want supervisor、worker", p.Profession)
	}
	if p.Description != "车间主管" {
		t.Errorf("Description=%q want 车间主管", p.Description)
	}
	wantTraits := []string{"沉稳", "念旧", "务实"}
	if !reflect.DeepEqual(p.Traits, wantTraits) {
		t.Errorf("Traits=%v want %v", p.Traits, wantTraits)
	}
	if p.SpeechStyle != "话少稳重" {
		t.Errorf("SpeechStyle=%q want 话少稳重", p.SpeechStyle)
	}
}

func TestLoadDir_MissingSection(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-02.md", "## 名字\n小林\n")
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	p := profiles["H-02"]
	if p == nil {
		t.Fatal("H-02 missing")
	}
	if p.DisplayName != "小林" {
		t.Errorf("DisplayName=%q want 小林", p.DisplayName)
	}
	if p.Profession != "" {
		t.Errorf("Profession=%q want empty", p.Profession)
	}
	if p.Description != "" {
		t.Errorf("Description=%q want empty", p.Description)
	}
	if len(p.Traits) != 0 {
		t.Errorf("Traits=%v want empty", p.Traits)
	}
	if p.SpeechStyle != "" {
		t.Errorf("SpeechStyle=%q want empty", p.SpeechStyle)
	}
}

func TestLoadDir_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(profiles) != 0 {
		t.Fatalf("got %d profiles, want 0", len(profiles))
	}
}

func TestLoadDir_EmptyStringDir(t *testing.T) {
	profiles, err := LoadDir("")
	if err != nil {
		t.Fatalf("LoadDir empty: %v", err)
	}
	if len(profiles) != 0 {
		t.Fatalf("got %d profiles, want 0", len(profiles))
	}
}

func TestLoadDir_NonMDIgnored(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-01.md", "## 名字\n老陈\n")
	writeProfile(t, dir, "notes.txt", "ignore me")
	writeProfile(t, dir, "config.yaml", "key: value")
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("got %d profiles, want 1 (only .md)", len(profiles))
	}
	if _, ok := profiles["H-01"]; !ok {
		t.Error("H-01 missing")
	}
}

func TestLoadDir_TraitsSplitByDelim(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-01.md", "## 性格特质\n沉稳、念旧、务实\n")
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	want := []string{"沉稳", "念旧", "务实"}
	if got := profiles["H-01"].Traits; !reflect.DeepEqual(got, want) {
		t.Errorf("Traits=%v want %v", got, want)
	}
}

func TestLoadDir_TraitsMultiline(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-01.md", "## 性格特质\n沉稳\n念旧\n务实\n")
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	want := []string{"沉稳", "念旧", "务实"}
	if got := profiles["H-01"].Traits; !reflect.DeepEqual(got, want) {
		t.Errorf("Traits=%v want %v", got, want)
	}
}

func TestLoadDir_TraitsMixedDelimAndLines(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-01.md", "## 性格特质\n沉稳、念旧\n务实、重视工艺\n")
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	want := []string{"沉稳", "念旧", "务实", "重视工艺"}
	if got := profiles["H-01"].Traits; !reflect.DeepEqual(got, want) {
		t.Errorf("Traits=%v want %v", got, want)
	}
}

func TestLoadDir_UnknownSectionIgnored(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-01.md", "## 名字\n老陈\n\n## 备注\n这是备注\n\n## 职业\n主管\n")
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	p := profiles["H-01"]
	if p.DisplayName != "老陈" {
		t.Errorf("DisplayName=%q want 老陈", p.DisplayName)
	}
	if p.Profession != "主管" {
		t.Errorf("Profession=%q want 主管", p.Profession)
	}
}

func TestLoadDir_MalformedNoHeader(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-01.md", "这只是普通文本，没有二级标题。")
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	p := profiles["H-01"]
	if p == nil {
		t.Fatal("H-01 missing")
	}
	if p.DisplayName != "" || p.Profession != "" || p.Description != "" || len(p.Traits) != 0 || p.SpeechStyle != "" {
		t.Errorf("expected all-empty Profile, got %+v", p)
	}
}

func TestLoadDir_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-01.md", "## 名字\n老陈\n")
	writeProfile(t, dir, "H-02.md", "## 名字\n小林\n")
	writeProfile(t, dir, "H-03.md", "## 名字\n小赵\n")
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(profiles) != 3 {
		t.Fatalf("got %d profiles, want 3", len(profiles))
	}
	ids := make([]string, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	want := []string{"H-01", "H-02", "H-03"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("IDs=%v want %v", ids, want)
	}
}

func TestLoadDir_NonexistentDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := LoadDir(dir)
	if err == nil {
		t.Fatal("expected error for nonexistent dir, got nil")
	}
}

func TestLoadDir_AttrBands(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-01.md", "## 名字\n老陈\n\n## 属性分段\n疲劳: 40,70,90\n能量: 30,55,80\n")
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	p := profiles["H-01"]
	if p == nil {
		t.Fatal("H-01 profile missing")
	}
	if got := p.AttrBands["疲劳"]; got != [3]float64{40, 70, 90} {
		t.Errorf("疲劳 bands = %v, want [40 70 90]", got)
	}
	if got := p.AttrBands["能量"]; got != [3]float64{30, 55, 80} {
		t.Errorf("能量 bands = %v, want [30 55 80]", got)
	}
	if _, ok := p.AttrBands["关节磨损"]; ok {
		t.Errorf("关节磨损 should be absent (not declared), got %v", p.AttrBands["关节磨损"])
	}
}

func TestLoadDir_AttrBandsAbsent(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-01.md", "## 名字\n老陈\n")
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if profiles["H-01"].AttrBands != nil {
		t.Errorf("AttrBands should be nil when section absent, got %v", profiles["H-01"].AttrBands)
	}
}

func TestLoadDir_AttrBandsBadLinesSkipped(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-01.md", "## 属性分段\n"+
		"疲劳: 40,70,90\n"+ // valid
		"能量: 80,30\n"+ // wrong value count
		"关节磨损: 50,30,70\n"+ // non-ascending
		"余额: 10,20,30\n"+ // unknown attribute (余额 bands not supported)
		"疲劳x: 1,2,3\n"+ // unknown attribute
		"能量: a,b,c\n") // non-numeric
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	bands := profiles["H-01"].AttrBands
	if len(bands) != 1 {
		t.Fatalf("only 疲劳 should parse, got %v", bands)
	}
	if got := bands["疲劳"]; got != [3]float64{40, 70, 90} {
		t.Errorf("疲劳 bands = %v, want [40 70 90]", got)
	}
}

func TestLoadDir_AttrBandsChineseColon(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "H-01.md", "## 属性分段\n疲劳：40,70,90\n")
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if got := profiles["H-01"].AttrBands["疲劳"]; got != [3]float64{40, 70, 90} {
		t.Errorf("疲劳 bands (Chinese colon) = %v, want [40 70 90]", got)
	}
}
