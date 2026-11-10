package countrypack

import (
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/packs"
)

// markers are the phrases a box label carried while the tax specialist had
// not confirmed what it names; a labels-only version drops them (product
// owner, 2026-11-09).
var markers = []string{"pending the tax specialist", "from memory"}

// labelsOnly fails unless next is prev with only its return box labels
// changed, under prev's sign-off: the manifest differs only in its version
// (its header comment aside), every file but pack.yaml and returns.yaml
// is prev's byte for byte, returns.yaml differs from prev's only on label
// lines, and its returns are prev's once the labels are set aside. It
// returns next's boxes of returnType, which carry no marker.
func labelsOnly(t *testing.T, country, prevVersion, nextVersion, returnType string) []Box {
	t.Helper()
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	prev, next := lib.Get(country, prevVersion), lib.Get(country, nextVersion)
	if prev == nil || next == nil || next.Return(returnType) == nil {
		t.Fatalf("Get(%s, %s) and Get(%s, %s).Return(%s): got %v and %v, want both with the return", country, prevVersion, country, nextVersion, returnType, prev, next)
	}
	prevDir, nextDir := prev.Dir(), next.Dir()
	if !next.SignedOff() || !reflect.DeepEqual(next.Manifest.SignOff, prev.Manifest.SignOff) {
		t.Errorf("sign-off: got %+v, want %s's %+v", next.Manifest.SignOff, prevDir, prev.Manifest.SignOff)
	}
	if got := lib.Latest(country); got != next {
		t.Errorf("Latest(%s): got %s, want %s", country, got.Dir(), nextDir)
	}
	same := *next
	same.Manifest.Version = prev.Manifest.Version
	same.Returns = Returns{Returns: unlabelled(next.Returns.Returns)}
	was := *prev
	was.Returns = Returns{Returns: unlabelled(prev.Returns.Returns)}
	if !reflect.DeepEqual(&same, &was) {
		t.Errorf("%s without its version and labels: got other than %s, want it unchanged", nextDir, prevDir)
	}
	sameFiles(t, prevDir, nextDir, "pack.yaml", "returns.yaml")
	labelLinesOnly(t, prevDir+"/returns.yaml", nextDir+"/returns.yaml")
	boxes := next.Return(returnType).Boxes
	for _, b := range boxes {
		for _, m := range markers {
			if strings.Contains(b.Label, m) {
				t.Errorf("box %s's label %q: got the marker %q, want none", b.Code, b.Label, m)
			}
		}
	}
	return boxes
}

// unlabelled is defs with every box's label cleared, the slices copied.
func unlabelled(defs []ReturnDefinition) []ReturnDefinition {
	out := slices.Clone(defs)
	for i := range out {
		out[i].Boxes = slices.Clone(out[i].Boxes)
		for j := range out[i].Boxes {
			out[i].Boxes[j].Label = ""
		}
	}
	return out
}

// labelLinesOnly fails unless the files have the same lines but for label
// lines, each still a label line at the same indentation: the bytes prove
// that nothing but a label string changed, comments and order included.
func labelLinesOnly(t *testing.T, prevFile, nextFile string) {
	t.Helper()
	was, errWas := fs.ReadFile(packs.FS, prevFile)
	is, errIs := fs.ReadFile(packs.FS, nextFile)
	if errWas != nil || errIs != nil {
		t.Fatalf("read: got %v and %v, want both files", errWas, errIs)
	}
	wasLines, isLines := strings.Split(string(was), "\n"), strings.Split(string(is), "\n")
	if len(wasLines) != len(isLines) {
		t.Fatalf("%s: got %d lines, want %s's %d", nextFile, len(isLines), prevFile, len(wasLines))
	}
	for i := range wasLines {
		if wasLines[i] == isLines[i] {
			continue
		}
		before, _, okWas := strings.Cut(wasLines[i], `label: "`)
		after, _, okIs := strings.Cut(isLines[i], `label: "`)
		if !okWas || !okIs || before != after || strings.TrimSpace(before) != "" {
			t.Errorf("%s line %d: got %q, want %q or the same label line with another label", nextFile, i+1, isLines[i], wasLines[i])
		}
	}
}

// UAE 2026.5 is 2026.4 with the VAT 201's box labels' markers dropped
// under 2026.4's sign-off (product owner, 2026-11-09): only label strings
// differ, and each box 1 label names the ISO 3166-2 code of the emirate
// its box selects. It is the latest UAE pack and applies without the note
// that no tax specialist signed it off.
func TestUAEPack2026_5(t *testing.T) {
	boxes := labelsOnly(t, "AE", "2026.4", "2026.5", "VAT201")
	var named int
	for _, b := range boxes {
		if b.Select == nil || len(b.Select.Jurisdictions) == 0 {
			continue
		}
		named++
		if code := b.Select.Jurisdictions[0]; len(b.Select.Jurisdictions) != 1 || !strings.Contains(b.Label, "("+code+")") {
			t.Errorf("box %s's label %q: got other than the one emirate it selects, want it naming %v", b.Code, b.Label, b.Select.Jurisdictions)
		}
	}
	if want := 3 * len(emirates); named != want {
		t.Errorf("boxes selecting an emirate: got %d, want %d", named, want)
	}
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	_, notes, err := Apply(companyIn(t, "AE", "AED"), lib.Get("AE", "2026.5"), lib, Options{Company: "1000", On: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), FiscalYear: 2026})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if hasNote(notes, "packs[1000]", "no tax specialist has signed this pack off") {
		t.Errorf("notes: got %v, want none saying the pack is not signed off", notes)
	}
}

// Canada 2026.8 is 2026.7 with the GST/HST return's line labels' markers
// dropped under 2026.7's sign-off (product owner, 2026-11-09): only label
// strings differ. It is the latest Canada pack.
func TestCanadaPack2026_8(t *testing.T) {
	boxes := labelsOnly(t, "CA", "2026.7", "2026.8", "GST34")
	i := slices.IndexFunc(boxes, func(b Box) bool { return b.Code == "112" })
	if want := "Line 110 plus line 111 (a paper return's subtotal)"; i < 0 || boxes[i].Label != want {
		t.Errorf("line 112: got %+v, want labelled %q", boxes, want)
	}
}
