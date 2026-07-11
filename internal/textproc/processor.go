// Package textproc buffers streaming assistant text and splits it into
// speakable sentences. It is a pure, non-concurrent state machine: the flush
// timer lives in the daemon's select loop, not here, so the monotonic seq
// counter stays race-free and is assigned at exactly one place in the pipeline.
// Mirrors Node src/text-processor.js (fence strip outside ```, noise filter,
// sentence-boundary split, markdown/URL/path cleaning) and fixes Node bug #15
// by inserting a block-seam separator so the splitter fires at block seams.
package textproc

import (
	"regexp"
	"strings"
)

// Sentence is a speakable unit. Seq is monotonic within the current epoch and
// is assigned here, at exactly one place in the whole pipeline.
type Sentence struct {
	Seq  uint64
	Text string
}

// Options tunes chunking and content filtering. Zero values fall back to the
// defaults below; nil/false Skip/Dedupe mean "no filtering".
type Options struct {
	MinChunkLen int      // default 10
	MaxChunkLen int      // default 500
	Skip        []string // drop any cleaned sentence containing one of these (case-insensitive)
	Dedupe      bool     // drop a cleaned sentence identical to the previous emitted one
}

const (
	defaultMinChunkLen = 10
	defaultMaxChunkLen = 500
)

// Precompiled patterns. RE2 has no lookaround, so the markdown-emphasis rules
// from Node (which relied on lookahead/lookbehind to guard snake_case and
// arithmetic) are rewritten to *consume* the flanking boundary characters and
// re-emit them via capture groups. Because a consumed trailing boundary can no
// longer serve as the leading boundary of an adjacent emphasis span, the two
// emphasis replacements are run to a fixpoint (see replaceStable).
var (
	// Sentence boundary: punctuation followed by whitespace. The punctuation is
	// the first byte of the match, mirroring Node's /([.!?])\s+/.
	reSentenceEnd = regexp.MustCompile(`[.!?]\s+`)

	// Bold: **...** (no newline in the span; . excludes \n in RE2 by default).
	reBold = regexp.MustCompile(`\*\*(.+?)\*\*`)

	// Italic *word* — only when the '*' is flanked by a non-word char / boundary
	// and the span starts and ends with a non-space, non-'*' byte. This keeps
	// "2 * 2" (space after '*') and bare '*' untouched.
	reItalicStar = regexp.MustCompile(`(^|\W)\*([^*\s](?:[^*\n]*?[^*\s])?)\*(\W|$)`)

	// Italic _word_ — same guard with '_'. The leading (^|\W) means a '_'
	// preceded by a word char (as in snake_case_var) never matches.
	reItalicUnderscore = regexp.MustCompile(`(^|\W)_([^_\s](?:[^_\n]*?[^_\s])?)_(\W|$)`)

	// Inline code span: `...`.
	reCodeSpan = regexp.MustCompile("`([^`]+)`")

	// URLs.
	reURL = regexp.MustCompile(`https?://\S+`)

	// File paths like /src/foo/bar.js or ~/notes.md, with a leading boundary.
	reFilePath = regexp.MustCompile(`(?:^|\s)[/~][\w./-]+`)

	// Runs of whitespace to collapse.
	reWhitespace = regexp.MustCompile(`\s+`)

	// A whole-line file path with a trailing :line-number, e.g. /a/b.js:42.
	reNoisePath = regexp.MustCompile(`^[/~][\w./-]+:\d+$`)
)

// Processor is a pure, non-concurrent state machine. The flush timer lives in
// the daemon's select loop, not here, so seq stays race-free.
type Processor struct {
	opts  Options
	buf   []byte
	seq   uint64
	last  string   // last emitted cleaned sentence, for --dedupe
	skips []string // lowercased, non-empty skip substrings (precomputed from opts.Skip)
}

// New builds a Processor, applying defaults for any zero-valued option and
// precomputing the lowercased skip substrings once.
func New(opts Options) *Processor {
	if opts.MinChunkLen == 0 {
		opts.MinChunkLen = defaultMinChunkLen
	}
	if opts.MaxChunkLen == 0 {
		opts.MaxChunkLen = defaultMaxChunkLen
	}
	p := &Processor{opts: opts}
	for _, s := range opts.Skip {
		if t := strings.ToLower(strings.TrimSpace(s)); t != "" {
			p.skips = append(p.skips, t)
		}
	}
	return p
}

// Feed strips fenced code blocks, drops noise, appends visible prose to the
// buffer inserting a block-seam separator (fix #15), splits on sentence
// boundaries + seams, cleans each for speech, and returns completed sentences.
func (p *Processor) Feed(text string) []Sentence {
	if text == "" {
		return nil
	}

	// Strip fenced code blocks (``` ... ```), keeping only the prose OUTSIDE
	// them. Split on ``` and walk the segments, omitting everything inside a
	// block (including the opening ```lang tag). Each feed is a COMPLETE
	// assistant content block with balanced fences, so the in-code state does
	// NOT carry across feeds — it resets every feed. A stray/unbalanced ```
	// therefore only mutes the rest of THIS block, not every later response.
	segments := strings.Split(text, "```")
	var vb strings.Builder
	inCode := false
	for i, seg := range segments {
		if !inCode {
			vb.WriteString(seg)
		}
		if i < len(segments)-1 {
			inCode = !inCode
		}
	}
	visible := vb.String()

	// Nothing speakable in this chunk (all code/empty) — nothing to buffer.
	if strings.TrimSpace(visible) == "" {
		return nil
	}

	// Filter out tool/noise content.
	if isNoise(visible) {
		return nil
	}

	// Fix Node bug #15 (run-on sentences at block seams): when the buffer
	// already holds a partial block, insert a separator before the new block so
	// a sentence-ending "." at a block's tail is followed by whitespace and the
	// splitter fires. A bare space only enables a split where the preceding
	// block already ended in [.!?]; it never manufactures a false boundary.
	if len(p.buf) > 0 {
		p.buf = append(p.buf, ' ')
	}
	p.buf = append(p.buf, visible...)

	return p.tryFlush()
}

// tryFlush splits the buffer at sentence boundaries, emits completed sentences,
// keeps the trailing partial in the buffer, and force-breaks an over-long
// buffer at the last space before MaxChunkLen. Mirrors Node _tryFlush: at most
// one max-length break per call.
func (p *Processor) tryFlush() []Sentence {
	var out []Sentence
	buf := string(p.buf)

	lastIndex := 0
	for _, loc := range reSentenceEnd.FindAllStringIndex(buf, -1) {
		// loc[0] is the punctuation byte; include it in the sentence. loc[1] is
		// past the trailing whitespace, where the next sentence begins.
		sentence := strings.TrimSpace(buf[lastIndex : loc[0]+1])
		if len(sentence) >= p.opts.MinChunkLen {
			if s, ok := p.emit(sentence); ok {
				out = append(out, s)
			}
		}
		lastIndex = loc[1]
	}
	if lastIndex > 0 {
		buf = buf[lastIndex:]
	}

	// Force flush if the remaining buffer exceeds max — break at the last space
	// at or before MaxChunkLen when that space is past MinChunkLen, else hard
	// cut at MaxChunkLen.
	if len(buf) >= p.opts.MaxChunkLen {
		lastSpace := strings.LastIndexByte(buf[:p.opts.MaxChunkLen+1], ' ')
		breakAt := p.opts.MaxChunkLen
		if lastSpace > p.opts.MinChunkLen {
			breakAt = lastSpace
		}
		chunk := strings.TrimSpace(buf[:breakAt])
		buf = buf[breakAt:]
		if len(chunk) >= p.opts.MinChunkLen {
			if s, ok := p.emit(chunk); ok {
				out = append(out, s)
			}
		}
	}

	p.buf = []byte(buf)
	return out
}

// emit cleans a candidate sentence for speech and, if it survives every filter,
// assigns the next monotonic seq and returns it. Any drop (too short, skip-match,
// or duplicate) happens BEFORE seq++ so it consumes no seq: the audio queue plays
// strictly in seq order, and a gap would stall the ordered drain forever.
func (p *Processor) emit(text string) (Sentence, bool) {
	cleaned := cleanForSpeech(text)
	if len(cleaned) < p.opts.MinChunkLen {
		return Sentence{}, false
	}
	if p.skipMatch(cleaned) {
		return Sentence{}, false
	}
	if p.opts.Dedupe && strings.EqualFold(cleaned, p.last) {
		return Sentence{}, false
	}
	p.last = cleaned
	p.seq++
	return Sentence{Seq: p.seq, Text: cleaned}, true
}

// skipMatch reports whether cleaned contains any configured skip substring
// (case-insensitive). Empty skip list is the common case and returns fast.
func (p *Processor) skipMatch(cleaned string) bool {
	if len(p.skips) == 0 {
		return false
	}
	lc := strings.ToLower(cleaned)
	for _, s := range p.skips {
		if strings.Contains(lc, s) {
			return true
		}
	}
	return false
}

// Flush emits whatever remains (if >= MinChunkLen) as a final sentence.
func (p *Processor) Flush() []Sentence {
	trimmed := strings.TrimSpace(string(p.buf))
	var out []Sentence
	if len(trimmed) >= p.opts.MinChunkLen {
		if s, ok := p.emit(trimmed); ok {
			out = append(out, s)
		}
	}
	p.buf = nil
	return out
}

// Reset clears the buffer, zeroes seq, and forgets the last emitted sentence
// (called on session switch).
func (p *Processor) Reset() {
	p.buf = nil
	p.seq = 0
	p.last = ""
}

// HasPending reports whether buffered text is awaiting a flush (arms the timer).
func (p *Processor) HasPending() bool {
	return len(p.buf) > 0
}

// cleanForSpeech strips markdown emphasis, code spans, URLs and file paths, and
// collapses whitespace. Emphasis regexes are snake_case/arithmetic-safe.
func cleanForSpeech(text string) string {
	// Bold first, then single-marker italics (run to a fixpoint so adjacent
	// spans that shared a boundary character are both stripped).
	text = reBold.ReplaceAllString(text, `${1}`)
	text = replaceStable(reItalicStar, text, `${1}${2}${3}`)
	text = replaceStable(reItalicUnderscore, text, `${1}${2}${3}`)
	text = reCodeSpan.ReplaceAllString(text, `${1}`)
	text = reURL.ReplaceAllString(text, "")
	text = reFilePath.ReplaceAllString(text, " ")
	text = reWhitespace.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

// replaceStable applies re→repl repeatedly until the string stops changing.
// Each replacement strictly removes emphasis markers, so the string only ever
// shrinks and the loop terminates; the iteration cap is a belt-and-braces
// guard against any pathological input.
func replaceStable(re *regexp.Regexp, s, repl string) string {
	for i := 0; i < 16; i++ {
		next := re.ReplaceAllString(s, repl)
		if next == s {
			break
		}
		s = next
	}
	return s
}

// isNoise reports whether a visible chunk is tool/JSON/path noise to skip.
func isNoise(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		return true
	}
	if strings.HasPrefix(trimmed, "Tool:") || strings.HasPrefix(trimmed, "tool_use") {
		return true
	}
	if reNoisePath.MatchString(trimmed) {
		return true
	}
	return false
}
