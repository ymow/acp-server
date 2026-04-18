package gittwin

import "testing"

func TestLinesAdded(t *testing.T) {
	m := BuiltinMappers["lines_added"]
	n, err := m(DiffStat{LinesAdded: 42, LinesRemoved: 10})
	if err != nil || n != 42 {
		t.Fatalf("want 42 got=%d err=%v", n, err)
	}
}

func TestLinesAddedMinusRemoved(t *testing.T) {
	m := BuiltinMappers["lines_added_minus_removed"]
	// positive net
	n, _ := m(DiffStat{LinesAdded: 42, LinesRemoved: 10})
	if n != 32 {
		t.Fatalf("net: want 32 got %d", n)
	}
	// negative net floors at 0
	n, _ = m(DiffStat{LinesAdded: 5, LinesRemoved: 20})
	if n != 0 {
		t.Fatalf("floor: want 0 got %d", n)
	}
}

func TestWordsInDiff(t *testing.T) {
	diff := `diff --git a/foo.md b/foo.md
--- a/foo.md
+++ b/foo.md
@@ -1,2 +1,4 @@
 unchanged line
+hello world, this is added
+another line with five words
-removed line with four words`
	n, err := wordsInDiff(DiffStat{DiffBody: diff})
	if err != nil {
		t.Fatal(err)
	}
	// "hello world this is added" = 5, "another line with five words" = 5
	// --- and +++ headers skipped; "removed" line not counted.
	if n != 10 {
		t.Fatalf("want 10 got %d", n)
	}
}

func TestWordsInDiffEmpty(t *testing.T) {
	n, _ := wordsInDiff(DiffStat{})
	if n != 0 {
		t.Fatalf("empty diff should produce 0, got %d", n)
	}
}

func TestResolveUnknown(t *testing.T) {
	if _, err := Resolve("not_a_mapper"); err == nil {
		t.Fatal("expected error for unknown mapper")
	}
	if _, err := Resolve(""); err == nil {
		t.Fatal("expected error for empty mapper name")
	}
}

func TestDefaultMapperForSpace(t *testing.T) {
	cases := map[string]struct {
		name string
		ok   bool
	}{
		"book":     {"words_in_diff", true},
		"code":     {"lines_added_minus_removed", true},
		"research": {"lines_added", true},
		"music":    {"", false},
		"custom":   {"", false},
		"unknown":  {"", false},
	}
	for space, want := range cases {
		got, ok := DefaultMapperForSpace(space)
		if got != want.name || ok != want.ok {
			t.Errorf("space=%s want=(%q,%v) got=(%q,%v)", space, want.name, want.ok, got, ok)
		}
	}
}
