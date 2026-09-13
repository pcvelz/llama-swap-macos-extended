package server

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// imageOmittedText is what a text-only model reads in place of an image
// block. It names the model and says what to do instead, so an agentic client
// that sent the image by accident (a Read of a screenshot, a pasted picture)
// learns the boundary from the reply rather than from an opaque 500.
func imageOmittedText(model string) string {
	return fmt.Sprintf("[image omitted: model %s accepts text only, so llama-swap replaced an image block with this note. Do not send images to this model; describe the content in text instead.]", model)
}

// replaceImageBlocks rewrites every image content block in body.messages into
// a text block carrying imageOmittedText. It handles the Anthropic shape
// (`{"type":"image",...}`) and the OpenAI shape (`{"type":"image_url",...}`),
// and descends into `tool_result` blocks, whose `content` may itself be a
// block array - that nesting is exactly how Claude Code returns a Read of a
// PNG, so a top-level-only walk would miss the witnessed case.
//
// WHY replace rather than reject: llama-server without an mmproj answers
// `500 image input is not supported`, and the block then sits in the client's
// conversation history, so every retry fails identically and an agent session
// is wedged until a human quits it. Only the proxy sees every request; turning
// the block into text is the one place the history can be healed.
//
// WHY the caller gates on declared capabilities: the proxy must never guess at
// a model's modalities. A model that declares nothing keeps its image blocks
// (upstream decides); a model that declares capabilities without "image" in
// `in` has stated it cannot take them, and the proxy honours that statement.
func replaceImageBlocks(body []byte, model string) ([]byte, error) {
	// Cheap byte pre-check: almost no request carries an image block, and an
	// unmarshal of a deep-context body on every turn is not free. `"image`
	// also matches `"image_url"` and `"image/png"`, all of which only ever
	// appear alongside an image block.
	if !bytes.Contains(body, []byte(`"image`)) {
		return body, nil
	}
	raw := gjson.GetBytes(body, "messages")
	if !raw.Exists() || !raw.IsArray() {
		return body, nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw.Raw)))
	// UseNumber keeps integers in tool inputs byte-identical on re-marshal.
	dec.UseNumber()
	var msgs []any
	if err := dec.Decode(&msgs); err != nil {
		// Malformed messages: leave the body alone and let upstream reject it
		// with its own error rather than masking it here.
		return body, nil
	}
	placeholder := imageOmittedText(model)
	changed := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		replaceImageBlocksIn(mm["content"], placeholder, &changed)
	}
	if !changed {
		return body, nil
	}
	return sjson.SetBytes(body, "messages", msgs)
}

// replaceImageBlocksIn walks one `content` value (string or block array),
// rewriting image blocks in place and recursing into tool_result content.
func replaceImageBlocksIn(content any, placeholder string, changed *bool) {
	blocks, ok := content.([]any)
	if !ok {
		return
	}
	for _, b := range blocks {
		blk, ok := b.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := blk["type"].(string)
		switch typ {
		case "image", "image_url":
			for k := range blk {
				delete(blk, k)
			}
			blk["type"] = "text"
			blk["text"] = placeholder
			*changed = true
		case "tool_result":
			replaceImageBlocksIn(blk["content"], placeholder, changed)
		}
	}
}
