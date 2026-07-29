package transform

import (
	"strconv"
	"strings"
)

// geminiAllowedSchemaFields 是 functionDeclarations.parameters 的 JSON Schema 字段白名单。
var geminiAllowedSchemaFields = map[string]bool{ //nolint:gochecknoglobals
	"anyOf": true, "default": true, "description": true, "enum": true,
	"example": true, "format": true, "items": true,
	"maxItems": true, "maxLength": true, "maxProperties": true, "maximum": true,
	"minItems": true, "minLength": true, "minProperties": true, "minimum": true,
	"nullable": true, "pattern": true, "properties": true, "propertyOrdering": true,
	"required": true, "title": true, "type": true,
}

// schemaUnsupportedKeys 是 Vertex AI 原生 Schema 不支持、需剥离的 JSON-Schema 关键字。
var schemaUnsupportedKeys = map[string]bool{ //nolint:gochecknoglobals
	"$schema": true, "$id": true, "$defs": true, "$ref": true, "definitions": true,
	"additionalProperties": true, "patternProperties": true, "unevaluatedProperties": true,
	"dependentSchemas": true, "if": true, "then": true, "else": true,
	"allOf": true, "anyOf": true, "oneOf": true, "not": true,
	"title": true,
}

// nativeSchemaProperty 与 Vertex 原生 Schema 的 {key,value} JSON 结构一致。
// 使用连续结构体切片可避免为每个属性额外分配一个两元素 map。
type nativeSchemaProperty struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

type nativeSchemaProperties []nativeSchemaProperty

// cleanNativeFunctionParameters 在一次递归中用 Gemini 白名单清洗 JSON
// Schema，并转换为匿名 Vertex UI 端点需要的原生 Map-style Schema。
func cleanNativeFunctionParameters(schema any) any {
	switch s := schema.(type) {
	case []any:
		out := make([]any, len(s))
		for i, item := range s {
			out[i] = cleanNativeFunctionParameters(item)
		}
		return out
	case map[string]any:
		// 不按不受信任 schema 的总键数无限预分配；最终只会保留白名单字段。
		cleaned := make(map[string]any, min(len(s), len(geminiAllowedSchemaFields)))
		for key, value := range s {
			if !geminiAllowedSchemaFields[key] || schemaUnsupportedKeys[key] {
				continue
			}
			switch key {
			case "properties":
				if vm, ok := value.(map[string]any); ok {
					props := make(nativeSchemaProperties, 0, len(vm))
					for k, v := range vm {
						props = append(props, nativeSchemaProperty{
							Key: k, Value: cleanNativeFunctionParameters(v),
						})
					}
					cleaned[key] = props
					continue
				}
				cleaned[key] = value
			case "items":
				if _, ok := value.(map[string]any); ok {
					cleaned[key] = cleanNativeFunctionParameters(value)
					continue
				}
				cleaned[key] = value
			default:
				cleaned[key] = value
			}
		}
		cleaned["type"] = nativeSchemaType(cleaned["type"])
		convertNativeSchemaNumericConstraints(cleaned)
		return cleaned
	default:
		return schema
	}
}

// toNativeSchema 把标准 JSON Schema 转为 Vertex AI 匿名 UI 端点要求的原生 Map-style Schema。
func toNativeSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if schemaUnsupportedKeys[k] {
			continue
		}
		out[k] = v
	}

	out["type"] = nativeSchemaType(out["type"])

	if props, ok := out["properties"].(map[string]any); ok {
		nativeProps := make([]any, 0, len(props))
		for key, value := range props {
			converted := value
			if vm, ok := value.(map[string]any); ok {
				converted = toNativeSchema(vm)
			}
			nativeProps = append(nativeProps, map[string]any{"key": key, "value": converted})
		}
		out["properties"] = nativeProps
	}

	if items, ok := out["items"].(map[string]any); ok {
		out["items"] = toNativeSchema(items)
	}

	convertNativeSchemaNumericConstraints(out)

	return out
}

func nativeSchemaType(raw any) string {
	picked := ""
	switch value := raw.(type) {
	case []any:
		picked = "string"
		for _, item := range value {
			if candidate, ok := item.(string); ok && candidate != "null" {
				picked = candidate
				break
			}
		}
	case string:
		picked = value
	default:
		return "OBJECT"
	}
	switch picked {
	case "string", "STRING":
		return "STRING"
	case "integer", "INTEGER":
		return "INTEGER"
	case "number", "NUMBER":
		return "NUMBER"
	case "boolean", "BOOLEAN":
		return "BOOLEAN"
	case "array", "ARRAY":
		return "ARRAY"
	case "object", "OBJECT":
		return "OBJECT"
	default:
		upper := strings.ToUpper(picked)
		if validNativeSchemaType(upper) {
			return upper
		}
		return "STRING"
	}
}

func convertNativeSchemaNumericConstraints(schema map[string]any) {
	for _, field := range [...]string{
		"minItems", "maxItems", "minProperties", "maxProperties", "minLength", "maxLength",
	} {
		if v, ok := schema[field]; ok && v != nil {
			switch n := v.(type) {
			case float64:
				schema[field] = strconv.FormatFloat(n, 'f', 0, 64)
			case int:
				schema[field] = strconv.Itoa(n)
			case int64:
				schema[field] = strconv.FormatInt(n, 10)
			case string:
			}
		}
	}
}

func canonicalNativeSchema(schema any) bool {
	m, ok := schema.(map[string]any)
	typ, typeOK := m["type"].(string)
	if !ok || !typeOK || !validNativeSchemaType(typ) {
		return false
	}
	for key, value := range m {
		if schemaUnsupportedKeys[key] || strings.Contains(key, "_") {
			return false
		}
		switch key {
		case "properties":
			if !canonicalNativeProperties(value) {
				return false
			}
		case "items":
			if nested, ok := value.(map[string]any); ok && !canonicalNativeSchema(nested) {
				return false
			}
		case "minItems", "maxItems", "minProperties", "maxProperties", "minLength", "maxLength":
			if value != nil {
				if _, ok := value.(string); !ok {
					return false
				}
			}
		}
	}
	return true
}

func canonicalNativeProperties(value any) bool {
	switch value.(type) {
	case nativeSchemaProperties, []any:
		return true
	default:
		return false
	}
}

func validNativeSchemaType(value string) bool {
	switch value {
	case "STRING", "INTEGER", "NUMBER", "BOOLEAN", "ARRAY", "OBJECT":
		return true
	default:
		return false
	}
}
