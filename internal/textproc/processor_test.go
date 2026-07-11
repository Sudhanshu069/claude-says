package textproc

import (
	"reflect"
	"testing"
)

// cleanForSpeech: markdown/URL/path stripping with the snake_case + arithmetic
// guards that motivated the RE2 rewrite.
func TestCleanForSpeech(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**bold** text", "bold text"},
		{"*italic* word", "italic word"},
		{"_under_ score", "under score"},
		{"see `code` here", "see code here"},
		{"**a** and *b* and _c_ and `d`", "a and b and c and d"},
		// snake_case identifiers must survive (underscores flanked by word chars).
		{"snake_case_var stays intact", "snake_case_var stays intact"},
		{"call do_the_thing now", "call do_the_thing now"},
		// arithmetic must survive ('*' flanked by spaces is not emphasis).
		{"compute 2 * 2 now", "compute 2 * 2 now"},
		// URLs and file paths are removed; whitespace collapses.
		{"go to https://example.com/x now", "go to now"},
		{"open /src/foo/bar.js now", "open now"},
		{"tilde ~/notes/todo.md gone", "tilde gone"},
		{"  many   spaces   here  ", "many spaces here"},
	}
	for _, c := range cases {
		if got := cleanForSpeech(c.in); got != c.want {
			t.Errorf("cleanForSpeech(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsNoise(t *testing.T) {
	noise := []string{
		"",
		"   ",
		`{"tool": "read"}`,
		"Tool: read file.js",
		"tool_use something",
		"/a/b/c.js:42",
		"~/x.go:1",
	}
	for _, s := range noise {
		if !isNoise(s) {
			t.Errorf("isNoise(%q) = false, want true", s)
		}
	}
	speak := []string{
		"This is a normal sentence.",
		"A path /a/b.js inside prose is fine.",
		"An object like {this} mid-sentence is fine.",
	}
	for _, s := range speak {
		if isNoise(s) {
			t.Errorf("isNoise(%q) = true, want false", s)
		}
	}
}

// Sentences split on [.!?]+whitespace, in order, with monotonic seq; the
// trailing partial stays buffered until a later terminator or Flush.
func TestSentenceSplitInOrderAndSeq(t *testing.T) {
	p := New(Options{})
	got := p.Feed("First sentence here. Second sentence here! Third partial")
	want := []Sentence{
		{Seq: 1, Text: "First sentence here."},
		{Seq: 2, Text: "Second sentence here!"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Feed sentences = %+v, want %+v", got, want)
	}
	if !p.HasPending() {
		t.Fatal("trailing partial should remain buffered")
	}
	if tail := p.Flush(); !reflect.DeepEqual(tail, []Sentence{{Seq: 3, Text: "Third partial"}}) {
		t.Fatalf("Flush tail = %+v, want seq 3 'Third partial'", tail)
	}
}

// Fix #15: consecutive assistant blocks get a seam separator so a '.' at a
// block's tail (with no trailing whitespace) still splits against the next block.
func TestBlockSeamSeparatorFix15(t *testing.T) {
	p := New(Options{})
	// First block ends in '.' with NO trailing whitespace -> nothing splits yet.
	if got := p.Feed("This is the first block."); len(got) != 0 {
		t.Fatalf("first feed emitted %+v, want nothing (trailing '.' with no space)", got)
	}
	// Second block: the seam separator makes ". " appear at the join, so the first
	// block now splits. Without the fix the buffer would be "...block.Second..."
	// and never split.
	got := p.Feed("Second block follows here.")
	want := []Sentence{{Seq: 1, Text: "This is the first block."}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("second feed = %+v, want %+v (seam split)", got, want)
	}
}

// Balanced fences: only prose OUTSIDE ``` is spoken; the code (and the ```lang
// tag) is dropped.
func TestFencedCodeStripped(t *testing.T) {
	p := New(Options{})
	got := p.Feed("Prose one. ```js\nsecret := code\n``` Prose two. ")
	want := []Sentence{
		{Seq: 1, Text: "Prose one."},
		{Seq: 2, Text: "Prose two."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fenced feed = %+v, want %+v (code muted)", got, want)
	}
	for _, s := range got {
		if s.Text == "secret := code" || s.Text == "js" {
			t.Fatalf("code leaked into speech: %q", s.Text)
		}
	}
}

// An ODD number of fences mutes only the rest of THAT feed; the in-code state
// resets on the next feed (so one stray ``` can't silence every later response).
func TestOddFenceMutesRestOfFeedThenResets(t *testing.T) {
	p := New(Options{})
	got1 := p.Feed("Before this. ```rest of this feed is muted code")
	if !reflect.DeepEqual(got1, []Sentence{{Seq: 1, Text: "Before this."}}) {
		t.Fatalf("odd-fence feed = %+v, want just 'Before this.'", got1)
	}
	// Next feed must behave normally (state reset), proving the mute didn't stick.
	if got2 := p.Feed("Next line here now."); len(got2) != 0 {
		t.Fatalf("second feed emitted %+v, want nothing yet (buffered)", got2)
	}
	tail := p.Flush()
	if !reflect.DeepEqual(tail, []Sentence{{Seq: 2, Text: "Next line here now."}}) {
		t.Fatalf("post-mute Flush = %+v, want seq 2 'Next line here now.' (state reset)", tail)
	}
}

// Whole-block noise (JSON / tool markers / path:line) is dropped and never
// buffered.
func TestNoiseFeedsDropped(t *testing.T) {
	p := New(Options{})
	for _, s := range []string{`{"a":1}`, "Tool: x", "/a/b.js:9"} {
		if got := p.Feed(s); got != nil {
			t.Errorf("Feed(%q) = %+v, want nil (noise)", s, got)
		}
	}
	if p.HasPending() {
		t.Fatal("noise must not be buffered")
	}
}

// A sentence that cleans DOWN below MinChunkLen is dropped WITHOUT consuming a
// seq: the following real sentence is still seq 1.
func TestSubMinAfterCleanDoesNotConsumeSeq(t *testing.T) {
	p := New(Options{})
	// "`abcdefgh`." is 11 raw bytes (passes the raw length gate) but cleans to
	// "abcdefgh." = 9 bytes (< 10), so it is dropped and consumes no seq. The
	// trailing space after the second sentence lets it split within this feed.
	got := p.Feed("`abcdefgh`. This sentence is plenty long here. ")
	if len(got) != 1 {
		t.Fatalf("got %+v, want exactly one sentence", got)
	}
	if got[0].Seq != 1 {
		t.Fatalf("seq = %d, want 1 (the sub-min sentence must not consume a seq)", got[0].Seq)
	}
}

// A long, unpunctuated buffer is force-broken at the last space before MaxChunkLen.
func TestMaxChunkForceBreak(t *testing.T) {
	p := New(Options{MinChunkLen: 5, MaxChunkLen: 40})
	got := p.Feed("word word word word word word word word word word extra")
	if len(got) != 1 {
		t.Fatalf("got %d sentences, want 1 force-broken chunk (%+v)", len(got), got)
	}
	if l := len(got[0].Text); l == 0 || l > 40 {
		t.Fatalf("force-broken chunk length = %d, want in (0,40]", l)
	}
	if !p.HasPending() {
		t.Fatal("remainder after the force break should stay buffered")
	}
}

// Flush emits the buffered tail; Reset clears the buffer and zeroes seq so
// numbering restarts (session-switch semantics).
func TestFlushAndReset(t *testing.T) {
	p := New(Options{})
	p.Feed("Partial without a terminator here")
	if !p.HasPending() {
		t.Fatal("expected pending buffer")
	}
	if got := p.Flush(); !reflect.DeepEqual(got, []Sentence{{Seq: 1, Text: "Partial without a terminator here"}}) {
		t.Fatalf("Flush = %+v, want seq 1 tail", got)
	}
	p.Reset()
	if p.HasPending() {
		t.Fatal("Reset must clear the buffer")
	}
	// seq restarts at 1 after Reset.
	if got := p.Flush(); got != nil {
		t.Fatalf("Flush after Reset = %+v, want nil", got)
	}
	// Trailing space so the sentence splits within this single feed.
	again := p.Feed("A fresh sentence after reset here. ")
	if len(again) != 1 || again[0].Seq != 1 {
		t.Fatalf("post-reset feed = %+v, want seq restart at 1", again)
	}
}

// --skip: a cleaned sentence containing a skip substring (case-insensitively) is
// dropped and — crucially — consumes no seq, so surviving sentences stay
// contiguously numbered. A seq gap would stall the audio queue's ordered drain.
func TestSkipDropsMatchAndKeepsSeqContiguous(t *testing.T) {
	p := New(Options{Skip: []string{"let me"}})
	// Middle sentence matches "let me" (via "Let Me", proving case-insensitivity)
	// and must vanish without leaving a seq hole between the other two.
	got := p.Feed("First real sentence here. Let Me check the config now. Second real sentence here. ")
	if len(got) != 2 {
		t.Fatalf("got %d sentences, want 2 (middle skipped): %+v", len(got), got)
	}
	if got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("seqs = %d,%d, want 1,2 (skipped sentence must not consume a seq)", got[0].Seq, got[1].Seq)
	}
	if got[0].Text != "First real sentence here." || got[1].Text != "Second real sentence here." {
		t.Fatalf("surviving text = %q,%q", got[0].Text, got[1].Text)
	}
}

// --dedupe: a cleaned sentence identical (case-insensitively) to the previous
// EMITTED one is dropped without consuming a seq; a later re-occurrence after a
// different sentence is spoken again (dedupe is consecutive-only).
func TestDedupeCollapsesConsecutiveOnly(t *testing.T) {
	p := New(Options{Dedupe: true})
	got := p.Feed("Running the tests now. Running the tests now. Something else entirely here. Running the tests now. ")
	if len(got) != 3 {
		t.Fatalf("got %d sentences, want 3 (one consecutive dup dropped): %+v", len(got), got)
	}
	if got[0].Seq != 1 || got[1].Seq != 2 || got[2].Seq != 3 {
		t.Fatalf("seqs = %d,%d,%d, want contiguous 1,2,3", got[0].Seq, got[1].Seq, got[2].Seq)
	}
	if got[0].Text != "Running the tests now." || got[1].Text != "Something else entirely here." || got[2].Text != "Running the tests now." {
		t.Fatalf("dedupe text = %+v (only the immediately-consecutive dup should drop)", got)
	}
}

// No filters configured => behaviour is unchanged (every sentence emitted).
func TestNoFiltersEmitsAll(t *testing.T) {
	p := New(Options{})
	got := p.Feed("One sentence here now. One sentence here now. ")
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (no dedupe unless enabled): %+v", len(got), got)
	}
}
