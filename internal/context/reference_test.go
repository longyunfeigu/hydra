package context

import (
	"testing"
)

func TestExtractSymbols_GoFunc(t *testing.T) {
	diff := `+func FooBar(x int) error {`

	got := ExtractSymbolsFromDiff(diff)

	if len(got) != 1 || got[0] != "FooBar" {
		t.Errorf("expected [FooBar], got: %v", got)
	}
}

func TestExtractSymbols_GoMethod(t *testing.T) {
	diff := `+func (s *Server) Handle(req Request) {`

	got := ExtractSymbolsFromDiff(diff)

	if len(got) != 1 || got[0] != "Handle" {
		t.Errorf("expected [Handle], got: %v", got)
	}
}

func TestExtractSymbols_JSFunction(t *testing.T) {
	diff := `+function processData(items) {`

	got := ExtractSymbolsFromDiff(diff)

	if !containsSymbol(got, "processData") {
		t.Errorf("expected processData in result, got: %v", got)
	}
}

func TestExtractSymbols_JSExport(t *testing.T) {
	diff := `+export function getData(id) {`

	got := ExtractSymbolsFromDiff(diff)

	if !containsSymbol(got, "getData") {
		t.Errorf("expected getData in result, got: %v", got)
	}
}

func TestExtractSymbols_ClassDef(t *testing.T) {
	diff := `+class MyService {`

	got := ExtractSymbolsFromDiff(diff)

	if !containsSymbol(got, "MyService") {
		t.Errorf("expected MyService in result, got: %v", got)
	}
}

func TestExtractSymbols_EmptyDiff(t *testing.T) {
	got := ExtractSymbolsFromDiff("")

	if len(got) != 0 {
		t.Errorf("expected empty slice for empty diff, got: %v", got)
	}
}

func TestExtractSymbols_NoSymbols(t *testing.T) {
	diff := `+    x := 42
+    y = "hello"
+    fmt.Println(x, y)`

	got := ExtractSymbolsFromDiff(diff)

	if len(got) != 0 {
		t.Errorf("expected no symbols for variable assignments, got: %v", got)
	}
}

func TestExtractSymbols_MultipleFunctions(t *testing.T) {
	diff := `+func Alpha(a int) {
+    // body
+}
+func (r *Repo) Beta(name string) error {
+    return nil
+}
+func Gamma() {`

	got := ExtractSymbolsFromDiff(diff)

	wantSymbols := []string{"Alpha", "Beta", "Gamma"}
	for _, sym := range wantSymbols {
		if !containsSymbol(got, sym) {
			t.Errorf("expected %s in result, got: %v", sym, got)
		}
	}

	if len(got) != len(wantSymbols) {
		t.Errorf("expected %d symbols, got %d: %v", len(wantSymbols), len(got), got)
	}
}

func TestExtractSymbols_OnlyAddedLines(t *testing.T) {
	// Lines starting with '-' should NOT be considered for symbol extraction.
	// Only '+' lines should be matched.
	diff := `-func RemovedFunc(x int) {
+func AddedFunc(y int) {`

	got := ExtractSymbolsFromDiff(diff)

	if containsSymbol(got, "RemovedFunc") {
		t.Errorf("should not extract symbols from deleted lines, got: %v", got)
	}

	if !containsSymbol(got, "AddedFunc") {
		t.Errorf("expected AddedFunc in result, got: %v", got)
	}
}

func containsSymbol(symbols []string, target string) bool {
	for _, s := range symbols {
		if s == target {
			return true
		}
	}
	return false
}
