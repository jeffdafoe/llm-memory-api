package memory

import (
	"strings"
	"unicode/utf8"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Chunk is one piece of a document, embedded and stored as a memory_chunks
// row. Heading is nil for a chunk with no heading.
type Chunk struct {
	Heading   *string `json:"heading"`
	ChunkText string  `json:"chunk_text"`
}

// isHeadingLine is /^#{1,3}\s/: one to three "#" and then whitespace.
func isHeadingLine(line string) bool {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n < 1 || n > 3 {
		return false
	}
	r, _ := utf8.DecodeRuneInString(line[n:])
	return n < len(line) && jsutil.IsSpace(r)
}

// hasProse reports whether lines hold a non-blank line that is not a
// heading: real prose under the heading, not stacked heading lines.
func hasProse(lines []string) bool {
	for _, l := range lines {
		if jsutil.Trim(l) != "" && !isHeadingLine(l) {
			return true
		}
	}
	return false
}

// ChunkByHeading splits markdown into one chunk per H1–H3 section.
//
// A section with no prose under its heading is not emitted: a "# Title" line
// followed straight by "## Section" would otherwise become a title-only
// chunk that out-ranks the note's real sections in search, and the recall
// tool renders it as an empty heading. A note made only of headings becomes
// one whole-note chunk, so it stays searchable.
func ChunkByHeading(content string) []Chunk {
	lines := strings.Split(content, "\n")
	chunks := []Chunk{}
	var heading *string
	var current []string

	flush := func() {
		if len(current) == 0 {
			return
		}
		text := jsutil.Trim(strings.Join(current, "\n"))
		if text != "" && hasProse(current) {
			chunks = append(chunks, Chunk{Heading: heading, ChunkText: text})
		}
	}

	for _, line := range lines {
		if isHeadingLine(line) {
			flush()
			h := jsutil.Trim(line)
			heading = &h
			current = []string{line}
		} else {
			current = append(current, line)
		}
	}
	flush()

	if len(chunks) == 0 {
		text := jsutil.Trim(content)
		if text != "" {
			var first *string
			for _, l := range lines {
				if isHeadingLine(l) {
					h := jsutil.Trim(l)
					first = &h
					break
				}
			}
			chunks = append(chunks, Chunk{Heading: first, ChunkText: text})
		}
	}
	return chunks
}

// isMessageLine reports the start of a conversation message line: a session
// log's "[HH:MM speaker] text" (/^\[\d{2}:\d{2}\s/) or a salem sim-day's
// "[Weekday HH:MM speaker] text".
func isMessageLine(line string) bool {
	if isClockAt(line, 1) {
		r, _ := utf8.DecodeRuneInString(line[6:])
		return jsutil.IsSpace(r)
	}
	at := weekdayClockAt(line)
	if at < 0 {
		return false
	}
	r, _ := utf8.DecodeRuneInString(line[at+5:])
	return jsutil.IsSpace(r)
}

// isClockAt reports whether line holds "HH:MM" starting at byte i, after a
// "[" at the start of the line.
func isClockAt(line string, i int) bool {
	return len(line) >= i+6 && line[0] == '[' && isDigit(line[i]) && isDigit(line[i+1]) &&
		line[i+2] == ':' && isDigit(line[i+3]) && isDigit(line[i+4])
}

// weekdayClockAt is the byte index of HH:MM in a line that starts
// "[Weekday HH:MM" (one or more ASCII letters, then one space), or -1.
func weekdayClockAt(line string) int {
	if len(line) == 0 || line[0] != '[' {
		return -1
	}
	i := 1
	for i < len(line) && isASCIILetter(line[i]) {
		i++
	}
	if i == 1 || i >= len(line) || line[i] != ' ' {
		return -1
	}
	if !isClockAt(line, i+1) {
		return -1
	}
	return i + 1
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// ChunkConversation splits a conversation log into windows of windowSize
// messages, carrying overlap messages into the next window. The header (the
// lines before the first message) is put in front of every chunk. With
// maxChars above 0 a window closes early once its text passes that many
// UTF-16 code units. A zero argument takes the default (5, 2, no limit).
func ChunkConversation(content string, windowSize, overlap, maxChars int) []Chunk {
	if windowSize == 0 {
		windowSize = 5
	}
	if overlap == 0 {
		overlap = 2
	}

	lines := strings.Split(content, "\n")
	var header, messageLines []string
	pastHeader := false
	for _, line := range lines {
		if !pastHeader && !isMessageLine(line) {
			header = append(header, line)
		} else {
			pastHeader = true
			messageLines = append(messageLines, line)
		}
	}

	// A message can span several lines.
	var messages []string
	var msg []string
	for _, line := range messageLines {
		if isMessageLine(line) && len(msg) > 0 {
			messages = append(messages, strings.Join(msg, "\n"))
			msg = []string{line}
		} else {
			msg = append(msg, line)
		}
	}
	if len(msg) > 0 {
		messages = append(messages, strings.Join(msg, "\n"))
	}

	if len(messages) == 0 {
		text := jsutil.Trim(content)
		if text != "" {
			return []Chunk{{ChunkText: text}}
		}
		return []Chunk{}
	}

	headerText := jsutil.Trim(strings.Join(header, "\n"))
	chunks := []Chunk{}
	start := 0
	for start < len(messages) {
		end := min(addSat(start, windowSize), len(messages))
		if maxChars > 0 {
			count := 0
			for i := start; i < end; i++ {
				count += jsutil.UTF16Len(messages[i])
				if count > maxChars && i > start {
					end = i
					break
				}
			}
		}
		// messages.slice(start, end): a negative end counts from the back
		// (a negative window size gets there), and an end not past start
		// gives nothing.
		sliceEnd := end
		if sliceEnd < 0 {
			sliceEnd = max(len(messages)+sliceEnd, 0)
		}
		var window []string
		if sliceEnd > start {
			window = messages[start:sliceEnd]
		}

		var parts []string
		if headerText != "" {
			parts = append(parts, headerText, "")
		}
		parts = append(parts, strings.Join(window, "\n"))
		text := jsutil.Trim(strings.Join(parts, "\n"))
		if text != "" {
			first := ""
			if len(window) > 0 {
				first = window[0]
			}
			var heading *string
			if len(first) >= 6 && first[0] == '[' && isDigit(first[1]) && isDigit(first[2]) &&
				first[3] == ':' && isDigit(first[4]) && isDigit(first[5]) {
				h := "Conversation at " + first[1:6]
				heading = &h
			} else if at := weekdayClockAt(first); at >= 0 {
				h := "Conversation at " + first[1:at+5]
				heading = &h
			}
			chunks = append(chunks, Chunk{Heading: heading, ChunkText: text})
		}

		// Advance by the window actually used (the char limit may have
		// closed it early) minus the overlap, and always by at least one.
		start += max(1, end-start-overlap)
	}
	return chunks
}

// MaxChunkChars caps one chunk's text, in UTF-16 code units. The embedding
// model rejects an input over 8192 tokens, and the whole ingest fails with
// it. English runs about 4 characters a token; 6000 leaves room for denser
// text (names, numbers, emoji) without a tokenizer.
const MaxChunkChars = 6000

// SplitOversized splits every chunk whose text is over maxChars into pieces
// that fit, each piece keeping the chunk's heading. Chunks that fit pass
// through unchanged, so a note with no oversized section chunks as before.
func SplitOversized(chunks []Chunk, maxChars int) []Chunk {
	out := make([]Chunk, 0, len(chunks))
	for _, c := range chunks {
		if jsutil.UTF16Len(c.ChunkText) <= maxChars {
			out = append(out, c)
			continue
		}
		for _, piece := range splitText(c.ChunkText, maxChars) {
			out = append(out, Chunk{Heading: c.Heading, ChunkText: piece})
		}
	}
	return out
}

// splitSeparators are tried coarsest first: paragraphs, then lines, then
// words. A piece still too long after words is cut by character count.
var splitSeparators = []string{"\n\n", "\n", " "}

// splitText breaks text into trimmed, non-empty pieces of at most maxChars.
// It packs as many whole parts per piece as fit, and goes to a finer
// separator only for a single part that is still too long.
func splitText(text string, maxChars int) []string {
	if jsutil.UTF16Len(text) <= maxChars {
		if t := jsutil.Trim(text); t != "" {
			return []string{t}
		}
		return nil
	}
	for _, sep := range splitSeparators {
		parts := strings.Split(text, sep)
		if len(parts) == 1 {
			continue
		}
		sepLen := jsutil.UTF16Len(sep)
		var pieces []string
		var current []string
		currentLen := 0
		emit := func() {
			if len(current) > 0 {
				pieces = append(pieces, splitText(strings.Join(current, sep), maxChars)...)
			}
			current = nil
			currentLen = 0
		}
		for _, part := range parts {
			partLen := jsutil.UTF16Len(part)
			if len(current) > 0 && currentLen+sepLen+partLen > maxChars {
				emit()
			}
			if len(current) > 0 {
				currentLen += sepLen
			}
			current = append(current, part)
			currentLen += partLen
		}
		emit()
		return pieces
	}
	return cutByChars(text, maxChars)
}

// cutByChars is the last resort for text with no separator: consecutive
// slices of at most maxChars UTF-16 code units, never splitting a rune.
func cutByChars(text string, maxChars int) []string {
	var pieces []string
	var b strings.Builder
	n := 0
	flush := func() {
		if t := jsutil.Trim(b.String()); t != "" {
			pieces = append(pieces, t)
		}
		b.Reset()
		n = 0
	}
	for _, r := range text {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if n+width > maxChars {
			flush()
		}
		b.WriteRune(r)
		n += width
	}
	flush()
	return pieces
}

// addSat adds without wrapping past the int range, as JavaScript numbers do
// not wrap.
func addSat(a, b int) int {
	s := a + b
	if b > 0 && s < a {
		return int(^uint(0) >> 1)
	}
	if b < 0 && s > a {
		return -int(^uint(0)>>1) - 1
	}
	return s
}
