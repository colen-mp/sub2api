package llmtelemetry

import (
	"regexp"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var mappingField = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:/-]{0,127}$`)

func validMappingField(name string) bool {
	if !mappingField.MatchString(name) {
		return false
	}
	for _, part := range strings.Split(name, ".") {
		if part == "" || part == "__proto__" || part == "constructor" || part == "prototype" {
			return false
		}
	}
	return true
}

// Legacy short aliases expand to Metadata. Full OTel keys pass through.
func mappingTargetKeys(target string) []string {
	if target == "user.id" || target == "langfuse.user.id" {
		return []string{"user.id", "langfuse.user.id"}
	}
	if strings.HasPrefix(target, "langfuse.") && strings.Count(target, ".") == 1 && target != "langfuse.version" && target != "langfuse.release" && target != "langfuse.environment" {
		alias := strings.TrimPrefix(target, "langfuse.")
		return []string{"langfuse.observation.metadata." + alias, "langfuse.trace.metadata." + alias}
	}
	return []string{target}
}

type mappedSpan struct {
	sdktrace.ReadOnlySpan
	attrs []attribute.KeyValue
}

func (s *mappedSpan) Attributes() []attribute.KeyValue { return s.attrs }

// Read only the completed, already-redacted Span attributes. There is no field
// allowlist, reflection into business entities, client input lookup, or chaining.
func mapAttributes(attrs []attribute.KeyValue, mapping map[string]string) []attribute.KeyValue {
	if len(mapping) == 0 {
		return attrs
	}
	original := make(map[string]attribute.Value, len(attrs))
	for _, a := range attrs {
		original[string(a.Key)] = a.Value
	}
	updates := map[string]attribute.Value{}
	identitySource := ""
	for source, target := range mapping {
		value, exists := original[source]
		if !exists {
			continue
		}
		switch target {
		case "user.id", "langfuse.user.id":
			name, ok := mappingScalar(value)
			if !ok {
				continue
			}
			value = attribute.StringValue(name)
			identitySource = strings.TrimPrefix(source, "sub2api.")
		case "langfuse.trace.tags":
			tags := []string{}
			if value.Type() == attribute.STRINGSLICE {
				if len(value.AsStringSlice()) > 64 {
					continue
				}
				for _, item := range value.AsStringSlice() {
					name := safeIdentityName(item)
					if name == "" {
						tags = nil
						break
					}
					tags = append(tags, name)
				}
			} else if name, ok := mappingScalar(value); ok {
				tags = append(tags, name)
			}
			if len(tags) == 0 {
				continue
			}
			value = attribute.StringSliceValue(tags)
		}
		for _, key := range mappingTargetKeys(target) {
			updates[key] = value
		}
	}
	if len(updates) == 0 {
		return attrs
	}
	if identitySource != "" {
		if _, explicit := updates["sub2api.user_identity_source"]; !explicit {
			updates["sub2api.user_identity_source"] = attribute.StringValue(identitySource)
		}
	}
	for key, value := range updates {
		original[key] = value
	}
	keys := make([]string, 0, len(original))
	for key := range original {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]attribute.KeyValue, 0, len(keys))
	for _, key := range keys {
		result = append(result, attribute.KeyValue{Key: attribute.Key(key), Value: original[key]})
	}
	return result
}

func mappingScalar(value attribute.Value) (string, bool) {
	switch value.Type() {
	case attribute.STRING, attribute.BOOL, attribute.INT64, attribute.FLOAT64:
		name := safeIdentityName(value.Emit())
		return name, name != ""
	}
	return "", false
}
