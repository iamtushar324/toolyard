package previewassertion

// Descriptor is a fixed, local catalog. No startup connection or caller-provided
// upstream URL is necessary. Toolyard's bridge adds its normal reason metadata.
type Descriptor struct {
	Name, Operation, Description string
	Schema                       map[string]any
	ReadOnly, Destructive        bool
}

func Descriptors() []Descriptor {
	text := func(maximum int) map[string]any {
		return map[string]any{"type": "string", "minLength": 1, "maxLength": maximum}
	}
	id := func() map[string]any {
		return map[string]any{"type": "string", "pattern": uuidRE.String()}
	}
	schema := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	return []Descriptor{
		{"bks_preview.create", "preview_create", "Queue a bounded isolated BKS branch, PR or draft preview. Queued is not ready. Caller/session come only from authenticated Toolyard context and the trusted operator binding.", schema(map[string]any{"source_ref": text(210), "request_id": id(), "snapshot_id": map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9-]{0,62}$"}, "pool": map[string]any{"type": "string", "enum": []string{"large", "medium"}, "default": "large"}, "lifetime_seconds": map[string]any{"type": "integer", "minimum": 300, "maximum": 21600, "default": 1200}}, "source_ref", "request_id", "snapshot_id"), false, false},
		{"bks_preview.inspect", "preview_inspect", "Inspect this authenticated owner's preview and immutable source identity.", schema(map[string]any{"environment_id": id()}, "environment_id"), true, false},
		{"bks_preview.heartbeat", "preview_heartbeat", "Report this caller's real active job. Does not extend the absolute deadline.", schema(map[string]any{"environment_id": id(), "job_id": id()}, "environment_id"), false, false},
		{"bks_preview.release", "preview_release", "Release this caller's ownership/job. Other valid owners keep the environment alive.", schema(map[string]any{"environment_id": id(), "outcome": map[string]any{"type": "string", "enum": []string{"released", "success", "failure"}, "default": "released"}, "job_id": id()}, "environment_id"), false, true},
		{"bks_preview.snapshots", "preview_snapshots", "List only reviewed synthetic snapshot IDs. No arbitrary S3 object is accepted.", schema(map[string]any{}), true, false},
		{"bks_preview.results", "preview_results", "Read owned durable test receipts. No raw application logs or arbitrary exec.", schema(map[string]any{"environment_id": id()}, "environment_id"), true, false},
	}
}
