package memwebhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// PayloadSpec lets each webhook (n8n job) structure its data differently inside
// its locked wing. It is the "schema flexibility" half of TEC-481: two webhooks
// bound to the same wing can map wildly different JSON shapes into a MemPalace
// entry without either touching the other.
//
// SECURITY: nothing in here can change the wing. The only caller-influenced
// routing is the topic *within* the locked wing, and only when TopicField is
// set — matching the guardrail "do not trust caller-supplied routing unless it
// is explicitly part of the webhook's allowed schema."
type PayloadSpec struct {
	// Mode selects how the entry text is built from the JSON payload:
	//   "whole"    (default) — pretty-printed full payload becomes the entry
	//   "field"    — the EntryField (dot-path) value becomes the entry
	//   "template" — EntryTemplate rendered with {{dot.path}} placeholders
	Mode          string `json:"mode,omitempty"`
	EntryField    string `json:"entry_field,omitempty"`
	EntryTemplate string `json:"entry_template,omitempty"`

	// Topic within the locked wing: a fixed Topic, or TopicField read from the
	// payload (TopicField wins when it resolves to a non-empty value).
	Topic      string `json:"topic,omitempty"`
	TopicField string `json:"topic_field,omitempty"`

	// RequiredFields are dot-paths that must be present and non-empty in the
	// payload. A miss is a schema violation (HTTP 422).
	RequiredFields []string `json:"required_fields,omitempty"`

	// JSONSchema, when set, is a JSON Schema (draft 2020-12 by default) the
	// payload is validated against before mapping. Invalid payloads are
	// rejected (HTTP 422).
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`

	// IncludeMetadataHeader prepends a compact provenance line to the stored
	// entry so the source webhook/request is visible inside MemPalace too.
	// Defaults to true when nil.
	IncludeMetadataHeader *bool `json:"include_metadata_header,omitempty"`
}

// Entry-building modes.
const (
	ModeWhole    = "whole"
	ModeField    = "field"
	ModeTemplate = "template"
)

var templateVar = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

// entryMeta carries the provenance metadata woven into the metadata header.
type entryMeta struct {
	WebhookName string
	RequestID   string
	Source      string
	Bytes       int64
}

// validateSpec checks a spec at create/update time so misconfiguration surfaces
// to the dashboard immediately rather than on the first ingest.
func validateSpec(spec *PayloadSpec) error {
	if spec == nil {
		return nil
	}
	switch effectiveMode(spec) {
	case ModeWhole:
	case ModeField:
		if strings.TrimSpace(spec.EntryField) == "" {
			return fmt.Errorf("%w: field mode requires entry_field", ErrInvalid)
		}
	case ModeTemplate:
		if strings.TrimSpace(spec.EntryTemplate) == "" {
			return fmt.Errorf("%w: template mode requires entry_template", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: mode must be whole|field|template", ErrInvalid)
	}
	if len(spec.JSONSchema) > 0 {
		if _, err := compileSchema(spec.JSONSchema); err != nil {
			return fmt.Errorf("%w: invalid json_schema: %v", ErrInvalid, err)
		}
	}
	return nil
}

func effectiveMode(spec *PayloadSpec) string {
	if spec == nil || strings.TrimSpace(spec.Mode) == "" {
		return ModeWhole
	}
	return spec.Mode
}

// buildEntry maps a raw JSON payload into a (entry, topic) pair according to the
// spec. It runs required-field and JSON-Schema validation first, so a bad
// payload is rejected as ErrSchemaViolation before anything is written.
func buildEntry(spec *PayloadSpec, raw json.RawMessage, meta entryMeta) (entry, topic string, err error) {
	if spec == nil {
		spec = &PayloadSpec{}
	}
	var payload any
	if len(bytes.TrimSpace(raw)) > 0 {
		if e := json.Unmarshal(raw, &payload); e != nil {
			return "", "", fmt.Errorf("%w: body is not valid JSON", ErrSchemaViolation)
		}
	}

	for _, f := range spec.RequiredFields {
		v, ok := lookupPath(payload, f)
		if !ok || isEmptyValue(v) {
			return "", "", fmt.Errorf("%w: required field %q is missing or empty", ErrSchemaViolation, f)
		}
	}

	if len(spec.JSONSchema) > 0 {
		if e := validateAgainstSchema(spec.JSONSchema, raw); e != nil {
			return "", "", fmt.Errorf("%w: %s", ErrSchemaViolation, e.Error())
		}
	}

	switch effectiveMode(spec) {
	case ModeWhole:
		b, e := json.MarshalIndent(payload, "", "  ")
		if e != nil {
			return "", "", fmt.Errorf("%w: payload not serializable", ErrSchemaViolation)
		}
		entry = string(b)
	case ModeField:
		v, ok := lookupPath(payload, spec.EntryField)
		if !ok {
			return "", "", fmt.Errorf("%w: entry_field %q not found in payload", ErrSchemaViolation, spec.EntryField)
		}
		entry = stringifyValue(v)
	case ModeTemplate:
		entry = renderTemplate(spec.EntryTemplate, payload)
	}

	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", "", fmt.Errorf("%w: mapped entry text is empty", ErrSchemaViolation)
	}

	if spec.TopicField != "" {
		if v, ok := lookupPath(payload, spec.TopicField); ok {
			topic = strings.TrimSpace(stringifyValue(v))
		}
	}
	if topic == "" {
		topic = strings.TrimSpace(spec.Topic)
	}

	if spec.IncludeMetadataHeader == nil || *spec.IncludeMetadataHeader {
		entry = metadataHeader(meta) + "\n\n" + entry
	}
	return entry, topic, nil
}

func metadataHeader(m entryMeta) string {
	src := m.Source
	if src == "" {
		src = "—"
	}
	return fmt.Sprintf("[via webhook %s · source %s · req %s · %d bytes]",
		m.WebhookName, src, m.RequestID, m.Bytes)
}

// lookupPath resolves a dot-path (e.g. "data.title") into a JSON value decoded
// to any. Only object traversal is supported; a path into a non-object returns
// not-found.
func lookupPath(root any, path string) (any, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, false
	}
	cur := root
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func renderTemplate(tmpl string, payload any) string {
	return templateVar.ReplaceAllStringFunc(tmpl, func(match string) string {
		sub := templateVar.FindStringSubmatch(match)
		if len(sub) != 2 {
			return match
		}
		if v, ok := lookupPath(payload, sub[1]); ok {
			return stringifyValue(v)
		}
		return ""
	})
}

// stringifyValue renders a JSON-decoded value as text: strings pass through,
// scalars format naturally, and composite values are re-marshalled.
func stringifyValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}

func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	default:
		return false
	}
}

// compileSchema compiles an in-memory JSON Schema document.
func compileSchema(rawSchema json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawSchema))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	const loc = "mem:///webhook-schema.json"
	if err := c.AddResource(loc, doc); err != nil {
		return nil, err
	}
	return c.Compile(loc)
}

// validateAgainstSchema validates a raw payload against a JSON Schema. The
// payload is decoded with jsonschema.UnmarshalJSON so numbers keep precision
// (json.Number), which the validator expects.
func validateAgainstSchema(rawSchema, rawPayload json.RawMessage) error {
	sch, err := compileSchema(rawSchema)
	if err != nil {
		return fmt.Errorf("schema did not compile: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawPayload))
	if err != nil {
		return fmt.Errorf("payload is not valid JSON: %w", err)
	}
	if err := sch.Validate(inst); err != nil {
		return err
	}
	return nil
}
