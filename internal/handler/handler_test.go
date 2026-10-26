package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aurora/internal/accounts"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	chatgpt_types "aurora/typings/chatgpt"
	officialtypes "aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// ─── Test: writeChatCompletionStreamDone ─────────────────────────

func TestWriteChatCompletionStreamDoneAddsStopBeforeDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)

	writeChatCompletionStreamDone(c, false, "auto", "conv-xxx")

	lines := sseDataLines(writer.Body.String())
	if len(lines) != 2 {
		t.Fatalf("data line count = %d, want 2; output: %s", len(lines), writer.Body.String())
	}
	var stopChunk map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &stopChunk); err != nil {
		t.Fatalf("invalid stop chunk: %v", err)
	}
	if stopChunk["conversation_id"] != "conv-xxx" {
		t.Fatalf("conversation_id = %#v, want conv-xxx", stopChunk["conversation_id"])
	}
	choices := stopChunk["choices"].([]interface{})
	if choices[0].(map[string]interface{})["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %#v, want stop", choices[0].(map[string]interface{})["finish_reason"])
	}
	if lines[1] != "[DONE]" {
		t.Fatalf("last data line = %q, want [DONE]", lines[1])
	}
}

func TestWriteChatCompletionStreamDoneSkipsDuplicateStop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)

	writeChatCompletionStreamDone(c, true, "auto", "conv-xxx")

	lines := sseDataLines(writer.Body.String())
	if len(lines) != 1 || lines[0] != "[DONE]" {
		t.Fatalf("data lines = %#v, want only [DONE]", lines)
	}
}

// ─── Test: toolCallingEnabled ────────────────────────────────────

func TestToolCallingEnabledFromConfig(t *testing.T) {
	okCfg := &config.Config{ToolCallingEnabled: true}
	disabledCfg := &config.Config{ToolCallingEnabled: false}

	if toolCallingEnabled(nil, okCfg) {
		t.Error("toolCallingEnabled(nil, true) should be false (len(nil)==0)")
	}
	if toolCallingEnabled(nil, disabledCfg) {
		t.Error("toolCallingEnabled(nil, false) should be false")
	}
	// empty tools slice with config enabled → false
	if toolCallingEnabled([]officialtypes.Tool{}, okCfg) {
		t.Error("toolCallingEnabled([], true) should be false")
	}
	// with actual tools and config enabled → true
	tools := []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "test"}}}
	if !toolCallingEnabled(tools, okCfg) {
		t.Error("toolCallingEnabled([tool], true) should be true")
	}
}

// ─── Test: original_requestHasFiles ──────────────────────────────

func TestOriginalRequestHasFiles(t *testing.T) {
	req := officialtypes.APIRequest{
		Messages: []officialtypes.APIMessage{
			{
				Role:    "user",
				Content: officialtypes.MessageContent{TextValue: "hello"},
			},
		},
	}
	if original_requestHasFiles(req) {
		t.Error("should be false when no files")
	}
}

// ─── Test: countMessagesTokens ───────────────────────────────────

func TestCountMessagesTokens(t *testing.T) {
	zero := countMessagesTokens(nil)
	if zero != 0 {
		t.Errorf("nil messages should return 0, got %d", zero)
	}
}

// ─── Test: resolveAccount ────────────────────────────────────────

func TestResolveAccountEmptyPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	pool := accounts.NewPool(nil)
	cfg := &config.Config{}

	acct, _, err := resolveAccount(c, pool, cfg, false)
	if err == nil {
		t.Fatal("expected error with empty pool")
	}
	if acct != nil {
		t.Fatal("expected nil account")
	}
}

func TestResolveAccountWithGlobalKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Request.Header.Set("Authorization", "Bearer my-global-key")

	pool := accounts.NewPool(nil)
	acct := accounts.NewAccount("test", accounts.TypeFree, "test-token")
	acct.Status = accounts.StatusActive
	pool.AddAccount(acct)
	cfg := &config.Config{Authorization: "my-global-key"}

	result, _, err := resolveAccount(c, pool, cfg, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected account, got nil")
	}
	if result.Token != "test-token" {
		t.Errorf("got token %q, want test-token", result.Token)
	}
}

func TestApplyClientStateToToolRequest(t *testing.T) {
	state := chatgpt.NewChatClientState()
	state.ConversationID = "conv-1"
	state.ParentMessageID = "msg-1"
	request := chatgpt_types.NewChatGPTRequest()

	applyClientStateToToolRequest(&request, state)

	if request.ConversationID != "conv-1" || request.ParentMessageID != "msg-1" {
		t.Fatalf("request state = conv %q parent %q", request.ConversationID, request.ParentMessageID)
	}
}

func TestSessionManagerRegistersResponsesState(t *testing.T) {
	manager := &SessionManager{
		sessions:         make(map[string]*sessionEntry),
		responseSessions: make(map[string]*responseSessionEntry),
		ttl:              defaultSessionTTL,
	}
	state := chatgpt.NewChatClientState()
	state.ConversationID = "conv-1"
	state.ParentMessageID = "msg-1"
	manager.RegisterResponse("resp-1", state, map[string]string{"call-1": "bash"})

	gotState, names := manager.GetResponse("resp-1")
	if gotState == state {
		t.Fatal("response state should be an immutable snapshot, not the original pointer")
	}
	if gotState.ConversationID != state.ConversationID || gotState.ParentMessageID != state.ParentMessageID {
		t.Fatalf("state = %#v, want %#v", gotState, state)
	}
	if names["call-1"] != "bash" {
		t.Fatalf("call name = %q, want bash", names["call-1"])
	}
}

func TestWriteResponsesToolCallingStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	h := &ChatHandler{}
	call := officialtypes.ToolCall{
		ID:   "call_123",
		Type: "function",
		Function: officialtypes.ToolCallFunc{
			Name:      "get_weather",
			Arguments: `{"city":"Paris"}`,
		},
	}
	response := officialtypes.NewResponsesResponseWithToolCalls("", "", []officialtypes.ToolCall{call}, 10, 5, 0, 0, 0, "gpt-5")

	h.writeResponsesToolCallingStream(c, response, []officialtypes.ToolCall{call})

	body := writer.Body.String()
	for _, event := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(body, event) {
			t.Fatalf("missing %q in %s", event, body)
		}
	}
	if strings.Contains(body, "chat.completion.chunk") || strings.Contains(body, `"choices"`) {
		t.Fatalf("Responses stream contains Chat Completions payload: %s", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("Responses stream should end with response.completed, not [DONE]: %s", body)
	}
	if strings.Count(body, response.ID) < 2 {
		t.Fatalf("response id %q was not reused across created/completed: %s", response.ID, body)
	}
}

func TestWriteResponsesToolCallingStreamWithFinalText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	h := &ChatHandler{}
	response := officialtypes.NewResponsesResponse("done", "", 10, 5, 0, 0, 0, "gpt-5")

	h.writeResponsesToolCallingStream(c, response, nil)

	body := writer.Body.String()
	for _, event := range []string{
		"event: response.output_item.added",
		"event: response.output_text.delta",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(body, event) {
			t.Fatalf("missing %q in %s", event, body)
		}
	}
	if !strings.Contains(body, `"text":"done"`) {
		t.Fatalf("final text message missing: %s", body)
	}
}

func sseDataLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		lines = append(lines, strings.TrimPrefix(line, "data: "))
	}
	return lines
}
