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

// isMessageLine is /^\[\d{2}:\d{2}\s/, the start of a "[HH:MM speaker] text"
// conversation line.
func isMessageLine(line string) bool {
	if len(line) < 7 || line[0] != '[' || !isDigit(line[1]) || !isDigit(line[2]) ||
		line[3] != ':' || !isDigit(line[4]) || !isDigit(line[5]) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(line[6:])
	return jsutil.IsSpace(r)
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
			}
			chunks = append(chunks, Chunk{Heading: heading, ChunkText: text})
		}

		// Advance by the window actually used (the char limit may have
		// closed it early) minus the overlap, and always by at least one.
		start += max(1, end-start-overlap)
	}
	return chunks
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
