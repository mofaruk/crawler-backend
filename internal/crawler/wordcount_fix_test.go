package crawler

import (
	"strings"
	"testing"
)

// A page whose only "content" is an inline script must still be recognised as
// thin. Counting script text let an analytics snippet or a JSON-LD block carry
// an empty page past the 100-word threshold, so the page was never reported.
func TestWordCountExcludesScriptAndStyle(t *testing.T) {
	html := `<html><head><title>Empty</title></head><body>
		<p>Two words</p>
		<script>
			var a = 1; var b = 2; console.log("lots of words in here indeed");
			function f(x) { return x + 1; } document.addEventListener("load", f);
		</script>
		<style>.a{color:red} .b{margin:0} .c{padding:0} .d{border:none}</style>
		<noscript>Please enable JavaScript to view this site properly</noscript>
	</body></html>`

	got := ExtractPageSignals(strings.NewReader(html), true)

	if got.WordCount != 2 {
		t.Errorf("WordCount = %d, want 2 — only the visible paragraph counts", got.WordCount)
	}
}

// Real body text either side of a script is still counted.
func TestWordCountResumesAfterScript(t *testing.T) {
	html := `<html><body>
		<p>one two three</p>
		<script>ignored ignored ignored ignored</script>
		<p>four five six</p>
	</body></html>`

	got := ExtractPageSignals(strings.NewReader(html), true)

	if got.WordCount != 6 {
		t.Errorf("WordCount = %d, want 6", got.WordCount)
	}
}
