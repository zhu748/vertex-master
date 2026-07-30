package vertex

import (
	"crypto/rand"
	"encoding/binary"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/transform"
)

// batchGraphql 请求的固定包壳常量（逐字节对齐 PoC body.json / vertex_client）。
const (
	querySignature = "2/l8eCsMMY49imcDQ/lwwXyL8cYtTjxZBF2dNqy69LodY="
	operationName  = "StreamGenerateContentAnonymous"
)

type batchRequestLocalization struct {
	Locale   string `json:"locale"`
	Timezone string `json:"timezone"`
}

// batchRequestContext 的字段顺序与 encoding/json 对原 map 的字典序一致，
// 既减少动态 map/接口装箱，也保持实际出站 JSON 的稳定顺序。
type batchRequestContext struct {
	BackendOverrides struct{}                 `json:"backendOverrides"`
	ClientSessionID  string                   `json:"clientSessionId"`
	ClientVersion    string                   `json:"clientVersion"`
	Jurisdiction     string                   `json:"jurisdiction"`
	LocalizationData batchRequestLocalization `json:"localizationData"`
	PagePath         string                   `json:"pagePath"`
	PageViewID       int64                    `json:"pageViewId"`
	SelectedPurview  struct{}                 `json:"selectedPurview"`
	TrackingID       string                   `json:"trackingId"`
}

func randomPageViewID() int64 {
	const minValue int64 = 1000000000000000
	const span uint64 = 9000000000000000
	// 拒绝顶部余数，避免直接取模产生分布偏差。
	spanValue := uint64(span)
	remainder := (-spanValue) % spanValue
	limit := ^uint64(0) - remainder
	var raw [8]byte
	for {
		if _, err := rand.Read(raw[:]); err != nil {
			return minValue
		}
		value := binary.LittleEndian.Uint64(raw[:])
		if value <= limit {
			return minValue + int64(value%spanValue)
		}
	}
}

func randomTrackingID() string {
	var output [17]byte
	fillRandomTrackingID(output[:])
	return string(output[:])
}

func fillRandomTrackingID(output []byte) {
	output[0] = 'd'
	position := 1
	var raw [32]byte
	for position < len(output) {
		if _, err := rand.Read(raw[:]); err != nil {
			for position < len(output) {
				output[position] = '0'
				position++
			}
			break
		}
		for _, value := range raw {
			// 250 是不超过 255 的最大 10 的倍数，拒绝其余值以保持数字均匀。
			if value >= 250 {
				continue
			}
			output[position] = '0' + value%10
			position++
			if position == len(output) {
				break
			}
		}
	}
}

func randomUUID() string {
	var output [36]byte
	fillRandomUUID(output[:])
	return string(output[:])
}

func fillRandomUUID(output []byte) {
	var uuid [16]byte
	_, _ = rand.Read(uuid[:])
	// Set version 4
	uuid[6] = (uuid[6] & 0x0f) | 0x40
	// Set variant to RFC4122
	uuid[8] = (uuid[8] & 0x3f) | 0x80
	const hexDigits = "0123456789ABCDEF"
	position := 0
	for index, value := range uuid {
		if index == 4 || index == 6 || index == 8 || index == 10 {
			output[position] = '-'
			position++
		}
		output[position] = hexDigits[value>>4]
		output[position+1] = hexDigits[value&0x0f]
		position += 2
	}
}

func randomRequestIdentifiers() (sessionID, trackingID string) {
	var output [53]byte
	fillRandomUUID(output[:36])
	fillRandomTrackingID(output[36:])
	combined := string(output[:])
	return combined[:36], combined[36:]
}

type batchGraphqlRequest struct {
	OperationName  string                `json:"operationName"`
	QuerySignature string                `json:"querySignature"`
	RequestContext batchRequestContext   `json:"requestContext"`
	Variables      batchRequestVariables `json:"variables"`
}

// batchRequestVariables mirrors the finite set produced by BuildVertexVariables.
// Keeping the shared prepared map out of the wire value avoids cloning it for every
// candidate merely to attach a candidate-specific recaptcha token.
//
// Fields stay in lexical order to preserve encoding/json's former map-key order.
type batchRequestVariables struct {
	Contents          any    `json:"contents,omitempty"`
	GenerationConfig  any    `json:"generationConfig,omitempty"`
	Model             any    `json:"model"`
	RecaptchaToken    string `json:"recaptchaToken"`
	Region            any    `json:"region"`
	SafetySettings    any    `json:"safetySettings,omitempty"`
	SystemInstruction any    `json:"systemInstruction,omitempty"`
	ToolConfig        any    `json:"toolConfig,omitempty"`
	Tools             any    `json:"tools,omitempty"`
}

// buildRequestPayload 构建发往上游的完整请求体（对齐 _build_request_payload）：
// 用 transform 构建 variables，再强制注入 region=global 与 recaptchaToken，最后包壳。
func buildRequestPayload(model string, geminiPayload map[string]any, recaptchaToken string, cfg config.ConfigProvider) batchGraphqlRequest {
	return buildRequestPayloadFromVariables(
		buildRequestVariables(model, geminiPayload, cfg),
		recaptchaToken,
	)
}

// buildRequestVariables 完成与候选节点、重试 token 无关的归一化。返回值发布后只读，
// 同一逻辑请求的多个候选和重试可安全共享。
func buildRequestVariables(model string, geminiPayload map[string]any, cfg config.ConfigProvider) map[string]any {
	vars := transform.BuildVertexVariables(model, geminiPayload, cfg)
	vars["region"] = "global"
	return vars
}

func buildRequestPayloadFromVariables(preparedVariables map[string]any, recaptchaToken string) batchGraphqlRequest {
	sessionID, trackingID := randomRequestIdentifiers()
	return batchGraphqlRequest{
		OperationName:  operationName,
		QuerySignature: querySignature,
		RequestContext: batchRequestContext{
			ClientSessionID: sessionID,
			ClientVersion:   "boq_cloud-boq-clientweb-vertexaistudio_20260630.00_p0",
			Jurisdiction:    "global",
			LocalizationData: batchRequestLocalization{
				Locale: "zh_CN", Timezone: "Asia/Hong_Kong",
			},
			PagePath:   "/agent-platform/studio/multimodal",
			PageViewID: randomPageViewID(),
			TrackingID: trackingID,
		},
		Variables: batchRequestVariables{
			Contents:          preparedVariables["contents"],
			GenerationConfig:  preparedVariables["generationConfig"],
			Model:             preparedVariables["model"],
			RecaptchaToken:    recaptchaToken,
			Region:            preparedVariables["region"],
			SafetySettings:    preparedVariables["safetySettings"],
			SystemInstruction: preparedVariables["systemInstruction"],
			ToolConfig:        preparedVariables["toolConfig"],
			Tools:             preparedVariables["tools"],
		},
	}
}
