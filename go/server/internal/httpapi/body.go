package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
)

// MaxBodyBytes is the JSON body limit ('5mb' in the Node server's
// express.json, which body-parser reads as 5 MiB).
const MaxBodyBytes = 5 * 1024 * 1024

// ParseJSONBody reads an application/json body into State.Body, with the same
// rules as Express's express.json() (body-parser):
//
//   - Only requests that carry a body (a Content-Length or Transfer-Encoding
//     header) and whose media type is application/json are read. Others pass
//     through with a nil Body.
//   - The body is decoded from its charset (default UTF-8) to UTF-8, a byte
//     order mark is dropped, and invalid UTF-8 bytes become U+FFFD.
//   - A zero-length body becomes {}. A body of only whitespace is malformed.
//   - The top-level value must be an object or array ("strict" mode).
//   - Over 5 MiB is 413. A charset that is not a known utf-* charset, or a
//     Content-Encoding other than identity, is 415. Malformed JSON is 400.
//
// A rejected body is answered by WriteUnhandledError and never reaches the
// routes or the request log, as in Node, where the parser's error skipped
// every later non-error middleware.
//
// Unlike body-parser, gzip and deflate request bodies are refused rather than
// inflated, and UTF-7 is refused rather than decoded; no client sends either.
func ParseJSONBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasBody(r) || !isJSONType(r.Header.Get("Content-Type")) {
			next.ServeHTTP(w, r)
			return
		}
		body, status, err := readJSONBody(r)
		if err != nil {
			WriteUnhandledError(w, status, err.Error())
			return
		}
		StateOf(r).Body = body
		next.ServeHTTP(w, r)
	})
}

// hasBody mirrors type-is's hasBody: a Transfer-Encoding header, or a
// Content-Length header that parses as a number (including "0").
func hasBody(r *http.Request) bool {
	if len(r.TransferEncoding) > 0 || r.Header.Get("Transfer-Encoding") != "" || r.ContentLength > 0 {
		return true
	}
	_, ok := r.Header["Content-Length"]
	return ok
}

func isJSONType(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

// charsetDecoders are the charsets body-parser (through iconv-lite) decodes
// that this server supports. UTF-16 and UTF-32 without an endianness use the
// byte order mark when there is one, and little-endian otherwise.
var charsetDecoders = map[string]encoding.Encoding{
	"utf-16":   unicode.UTF16(unicode.LittleEndian, unicode.UseBOM),
	"utf-16le": unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM),
	"utf-16be": unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM),
	"utf-32":   utf32.UTF32(utf32.LittleEndian, utf32.UseBOM),
	"utf-32le": utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM),
	"utf-32be": utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM),
}

func readJSONBody(r *http.Request) (json.RawMessage, int, error) {
	encodingHeader := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if encodingHeader != "" && encodingHeader != "identity" {
		return nil, http.StatusUnsupportedMediaType, fmt.Errorf("unsupported content encoding \"%s\"", encodingHeader)
	}
	_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	charset := strings.ToLower(params["charset"])
	if charset == "" {
		charset = "utf-8"
	}
	if !strings.HasPrefix(charset, "utf-") {
		return nil, http.StatusUnsupportedMediaType, fmt.Errorf("unsupported charset \"%s\"", strings.ToUpper(charset))
	}
	decoder, known := charsetDecoders[charset]
	if charset != "utf-8" && !known {
		return nil, http.StatusUnsupportedMediaType, fmt.Errorf("unsupported charset \"%s\"", strings.ToUpper(charset))
	}

	if r.ContentLength > MaxBodyBytes {
		return nil, http.StatusRequestEntityTooLarge, errors.New("request entity too large")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("request aborted")
	}
	if len(raw) > MaxBodyBytes {
		return nil, http.StatusRequestEntityTooLarge, errors.New("request entity too large")
	}
	if len(raw) == 0 {
		return json.RawMessage("{}"), 0, nil
	}

	if decoder != nil {
		decoded, err := decoder.NewDecoder().Bytes(raw)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		raw = decoded
	}
	text := replaceInvalidUTF8(raw)
	text = bytes.TrimPrefix(text, []byte("\uFEFF"))

	trimmed := bytes.TrimLeft(text, " \t\n\r")
	if len(trimmed) == 0 {
		// V8's message for JSON.parse of whitespace.
		return nil, http.StatusBadRequest, errors.New("Unexpected end of JSON input")
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return nil, http.StatusBadRequest, fmt.Errorf("Unexpected token '%c', body is not a JSON object or array", trimmed[0])
	}
	if !json.Valid(text) {
		var probe any
		message := "Invalid JSON body"
		if parseErr := json.Unmarshal(text, &probe); parseErr != nil {
			message = parseErr.Error()
		}
		return nil, http.StatusBadRequest, errors.New(message)
	}
	return json.RawMessage(text), 0, nil
}

// replaceInvalidUTF8 turns each byte that is not part of valid UTF-8 into
// U+FFFD, as Node's UTF-8 decoding of the body did.
func replaceInvalidUTF8(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	out := make([]byte, 0, len(b)+16)
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			out = append(out, "\uFFFD"...)
		} else {
			out = append(out, b[:size]...)
		}
		b = b[size:]
	}
	return out
}
