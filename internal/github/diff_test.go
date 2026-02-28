package github

import (
	"testing"
)

func TestParseDiffLines_SingleHunk(t *testing.T) {
	patch := `@@ -1,5 +1,7 @@ func example() {
 existing line
+new line 1
+new line 2
 another existing
 final line`

	got := ParseDiffLines(patch)

	// Right-side lines: 1 (context), 2 (+), 3 (+), 4 (context), 5 (context)
	want := map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true}

	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d lines\ngot: %v", len(got), len(want), got)
	}
	for line := range want {
		if !got[line] {
			t.Errorf("expected line %d in result, got: %v", line, got)
		}
	}
}

func TestParseDiffLines_MultipleHunks(t *testing.T) {
	patch := `@@ -1,3 +1,4 @@ package main
 line one
+added in first hunk
 line two
@@ -20,3 +21,4 @@ func other() {
 existing
+added in second hunk
 end`

	got := ParseDiffLines(patch)

	// First hunk: right lines 1 (context), 2 (+), 3 (context)
	// Second hunk: right lines 21 (context), 22 (+), 23 (context)
	wantLines := []int{1, 2, 3, 21, 22, 23}

	for _, line := range wantLines {
		if !got[line] {
			t.Errorf("expected line %d in result, got: %v", line, got)
		}
	}

	if len(got) != len(wantLines) {
		t.Errorf("got %d lines, want %d lines\ngot: %v", len(got), len(wantLines), got)
	}
}

func TestParseDiffLines_AdditionsOnly(t *testing.T) {
	patch := `@@ -0,0 +1,3 @@
+line one
+line two
+line three`

	got := ParseDiffLines(patch)

	want := map[int]bool{1: true, 2: true, 3: true}

	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d\ngot: %v", len(got), len(want), got)
	}
	for line := range want {
		if !got[line] {
			t.Errorf("expected line %d in result", line)
		}
	}
}

func TestParseDiffLines_DeletionsOnly(t *testing.T) {
	// All deletions: right-side counter doesn't advance for '-' lines,
	// but context lines around them do get tracked.
	patch := `@@ -1,4 +1,2 @@ func foo() {
 keep this
-removed line one
-removed line two
 keep that`

	got := ParseDiffLines(patch)

	// Right-side: line 1 (context " keep this"), then two '-' lines (no increment),
	// then line 2 (context " keep that")
	want := map[int]bool{1: true, 2: true}

	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d\ngot: %v", len(got), len(want), got)
	}
	for line := range want {
		if !got[line] {
			t.Errorf("expected line %d in result", line)
		}
	}
}

func TestParseDiffLines_EmptyPatch(t *testing.T) {
	got := ParseDiffLines("")

	if len(got) != 0 {
		t.Errorf("expected empty map for empty patch, got: %v", got)
	}
}

func TestParseDiffLines_ContextLines(t *testing.T) {
	// Context lines (starting with space) should be tracked as valid right-side lines.
	patch := `@@ -5,4 +5,4 @@ func bar() {
 context line A
 context line B
 context line C
 context line D`

	got := ParseDiffLines(patch)

	// All are context lines starting at right-side line 5
	want := map[int]bool{5: true, 6: true, 7: true, 8: true}

	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d\ngot: %v", len(got), len(want), got)
	}
	for line := range want {
		if !got[line] {
			t.Errorf("expected line %d in result", line)
		}
	}
}

func TestParseDiffLines_RealWorldDiff(t *testing.T) {
	patch := `@@ -10,6 +10,8 @@ func example() {
     existing line
+    new line 1
+    new line 2
     another existing
-    removed line
     final line`

	got := ParseDiffLines(patch)

	// Right-side tracking:
	// Line 10: " existing line" (context) -> tracked
	// Line 11: "+new line 1" (addition) -> tracked
	// Line 12: "+new line 2" (addition) -> tracked
	// Line 13: " another existing" (context) -> tracked
	// "-    removed line" -> deletion, no right-side increment
	// Line 14: " final line" (context) -> tracked
	wantLines := []int{10, 11, 12, 13, 14}

	for _, line := range wantLines {
		if !got[line] {
			t.Errorf("expected line %d in result, got: %v", line, got)
		}
	}

	if len(got) != len(wantLines) {
		t.Errorf("got %d lines, want %d\ngot: %v", len(got), len(wantLines), got)
	}
}
