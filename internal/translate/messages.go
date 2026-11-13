package translate

import (
	"strings"
)

// This file is the Messages module: what the translators share about writing
// a Messages request's blocks.

// imageBlock takes an image by URL, or inline from a base64 data URL.
func imageBlock(url, where string) (string, error) {
	if !strings.HasPrefix(url, "data:") {
		return set(`{"type":"image","source":{"type":"url"}}`, "source.url", url), nil
	}
	mediaType, data, ok := base64DataURL(url)
	if !ok {
		return "", refuse("%s: an image data URL must be base64", where)
	}
	block := set(`{"type":"image","source":{"type":"base64"}}`, "source.media_type", mediaType)
	return set(block, "source.data", data), nil
}

// documentBlock takes a file part's inline PDF; a file_id names a file at
// OpenAI, which the Claude API can't reach.
func documentBlock(part chatPart, where string) (string, error) {
	mediaType, data, ok := base64DataURL(part.fileData)
	if !ok || mediaType != "application/pdf" {
		return "", refuse("%s: the Claude API takes a file part only as an inline PDF in file_data", where)
	}
	return set(`{"type":"document","source":{"type":"base64","media_type":"application/pdf"}}`, "source.data", data), nil
}
