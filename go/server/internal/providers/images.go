package providers

import (
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// images.go — image parts in a user message (LLM-1040: mail attachments a
// virtual agent can see).
//
// A caller builds the message once, in OpenAI's chat shape, the shape
// RawOpts.Messages already uses:
//
//	{role: "user", content: [
//	    {type: "text", text: "..."},
//	    {type: "image_url", image_url: {url: "data:image/png;base64,..."}},
//	]}
//
// OpenAI's chat completions take that as it is. The other adapters
// translate each part into their own block when they build the request:
// /v1/responses (input_text / input_image), Anthropic (an image block with a
// base64 source), Google (inline_data). The mail side accepts PNG and JPEG
// only, which all of these read.

// Image is one picture for a user message: its media type and its bytes as
// standard base64.
type Image struct {
	MediaType  string
	DataBase64 string
}

// visionModels are, per adapter, the model families known to read images,
// by id prefix. An adapter alone is not enough: a model of it may be
// text-only, and OpenRouter routes to models of every kind, so it is not
// listed. The model registry carries no image-input flag to check instead.
var visionModels = map[string][]string{
	"openai":    {"gpt-4o", "gpt-4.1", "gpt-5", "gpt-6", "o1", "o3", "o4"},
	"anthropic": {"claude-"},
	"google":    {"gemini-"},
}

// textOnlyMarks are the parts of a model id that mark a variant within those
// families that does not take image input: speech, audio and realtime
// models, transcription, embeddings, search previews and live (streaming
// audio) models.
var textOnlyMarks = []string{"tts", "audio", "realtime", "transcribe", "embedding", "search", "live"}

// textOnlyPrefixes are older models of a vision family that predate images:
// Claude 1 and 2, and Claude Instant.
var textOnlyPrefixes = []string{"claude-1", "claude-2", "claude-instant"}

// SupportsImages reports whether the provider (a name or an alias) and model
// can be sent image parts: a vision family, and not a text-only variant.
func SupportsImages(provider, model string) bool {
	a, err := resolveProvider(provider)
	if err != nil || a == nil {
		return false
	}
	id := jsutil.ToLower(model)
	for _, mark := range textOnlyMarks {
		if strings.Contains(id, mark) {
			return false
		}
	}
	for _, prefix := range textOnlyPrefixes {
		if strings.HasPrefix(id, prefix) {
			return false
		}
	}
	for _, prefix := range visionModels[a.name] {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

// ImageUserMessage is one user message of the text followed by the images,
// in OpenAI's chat shape, for RawOpts.Messages.
func ImageUserMessage(text string, images []Image) *object {
	parts := []any{obj("type", "text", "text", text)}
	for _, img := range images {
		url := "data:" + img.MediaType + ";base64," + img.DataBase64
		parts = append(parts, obj("type", "image_url", "image_url", obj("url", url)))
	}
	return obj("role", "user", "content", parts)
}

// contentPart is one part of an array content, read back: its text, or its
// image as a media type and base64 data. ok is false for a part that is
// neither (left as it is by the translators).
type contentPart struct {
	isImage   bool
	text      any
	mediaType string
	data      string
}

func readPart(p any) (contentPart, bool) {
	o, isObj := p.(*object)
	if !isObj {
		return contentPart{}, false
	}
	switch {
	case isStr(o.Value("type"), "text"):
		return contentPart{text: o.Value("text")}, true
	case isStr(o.Value("type"), "image_url"):
		img, isImg := o.Value("image_url").(*object)
		if !isImg {
			return contentPart{}, false
		}
		url, isURL := img.Value("url").(string)
		if !isURL {
			return contentPart{}, false
		}
		mediaType, data, isData := splitDataURL(url)
		if !isData {
			return contentPart{}, false
		}
		return contentPart{isImage: true, mediaType: mediaType, data: data}, true
	}
	return contentPart{}, false
}

// splitDataURL reads "data:<media type>;base64,<data>".
func splitDataURL(url string) (mediaType, data string, ok bool) {
	rest, found := strings.CutPrefix(url, "data:")
	if !found {
		return "", "", false
	}
	head, data, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	mediaType, found = strings.CutSuffix(head, ";base64")
	if !found || mediaType == "" {
		return "", "", false
	}
	return mediaType, data, true
}

// translateParts maps each part of an array content through text and image;
// a part readPart does not know is kept as it is. A content that is not an
// array, or an array with no image part, is returned unchanged, so a
// caller's existing text-part arrays (cache markers and all) go out exactly
// as before.
func translateParts(content any, text func(any) any, image func(mediaType, data string) any) any {
	parts, isArr := arr(content)
	if !isArr || !hasImagePart(parts) {
		return content
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		cp, known := readPart(p)
		switch {
		case !known:
			out = append(out, p)
		case cp.isImage:
			out = append(out, image(cp.mediaType, cp.data))
		default:
			out = append(out, text(cp.text))
		}
	}
	return out
}

func hasImagePart(parts []any) bool {
	for _, p := range parts {
		if cp, known := readPart(p); known && cp.isImage {
			return true
		}
	}
	return false
}

// responsesContent is a user message's content for /v1/responses.
func responsesContent(content any) any {
	return translateParts(content,
		func(t any) any { return obj("type", "input_text", "text", t) },
		func(mediaType, data string) any {
			return obj("type", "input_image", "image_url", "data:"+mediaType+";base64,"+data, "detail", "auto")
		})
}

// anthropicContent is a user message's content for Anthropic's messages API.
func anthropicContent(content any) any {
	return translateParts(content,
		func(t any) any { return obj("type", "text", "text", t) },
		func(mediaType, data string) any {
			return obj("type", "image", "source", obj("type", "base64", "media_type", mediaType, "data", data))
		})
}

// googleUserParts is a user message's parts for Gemini: the text as one part
// (as before), or, for array content, a part per text and image.
func googleUserParts(content any) []any {
	if parts, isArr := arr(content); !isArr || !hasImagePart(parts) {
		return []any{obj("text", or(content, ""))}
	}
	parts, _ := arr(translateParts(content,
		func(t any) any { return obj("text", t) },
		func(mediaType, data string) any {
			return obj("inline_data", obj("mime_type", mediaType, "data", data))
		}))
	return parts
}
