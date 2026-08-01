package ai

import "encoding/json"

// generationSchema is the provider-facing schema. Anthropic's constrained
// decoder supports structural JSON Schema keywords but not length or range
// constraints in raw schemas. Those constraints stay in ValidateResult, while
// descriptions preserve the intent for the model.
const generationSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "mode": {
      "type": "string",
      "enum": ["reply", "comment"],
      "description": "Whether the text answers a person directly or is a standalone public comment under a post."
    },
    "mode_confidence": {
      "type": "string",
      "enum": ["high", "low"],
      "description": "High when one scenario clearly dominates; low only when the user must choose before publication."
    },
    "situation": {
      "type": "string",
      "description": "A non-empty neutral summary of the social situation, no longer than 500 characters."
    },
    "replies": {
      "type": "array",
      "description": "Exactly three distinct send-ready replies.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "tone": {
            "type": "string",
            "enum": ["smart", "playful", "sharp", "boundary", "meme"]
          },
          "text": {
            "type": "string",
            "description": "A non-empty reply no longer than 300 characters."
          },
          "note": {
            "type": "string",
            "description": "An optional note no longer than 160 characters."
          }
        },
        "required": ["tone", "text"]
      }
    },
    "meme": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "headline": {"type": "string", "description": "A non-empty headline no longer than 120 characters."},
        "caption": {"type": "string", "description": "A non-empty caption no longer than 220 characters."},
        "footer": {"type": "string", "description": "An optional footer no longer than 120 characters."},
        "mood": {"type": "string", "description": "An optional mood label no longer than 40 characters."}
      },
      "required": ["headline", "caption"]
    }
  },
  "required": ["mode", "mode_confidence", "situation", "replies"]
}`

// GenerationJSONSchema returns an owned copy of the schema. Returning a copy
// prevents callers from mutating data used by later requests.
func GenerationJSONSchema() json.RawMessage {
	return append(json.RawMessage(nil), generationSchema...)
}
