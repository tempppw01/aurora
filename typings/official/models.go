package official

// Model 对应 OpenAI /v1/models 列表中的单个模型条目。
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelsResponse 对应 /v1/models 的响应体。
type ModelsResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}
