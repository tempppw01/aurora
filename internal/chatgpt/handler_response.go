package chatgpt

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"aurora/conversion/response/chatgpt"
	"aurora/httpclient"
	"aurora/internal/accounts"
	"aurora/internal/sseparser"
	"aurora/typings"
	chatgpt_types "aurora/typings/chatgpt"
	official_types "aurora/typings/official"

	"github.com/bogdanfinn/websocket"
)

type conversationPatchState struct {
	response chatgpt_types.ChatGPTResponse
	channel  string
}

type conversationStreamEvent struct {
	response       chatgpt_types.ChatGPTResponse
	chunk          *official_types.ChatCompletionChunk
	text           string
	role           string
	conversationID string
	messageID      string
	channel        string
	finishReason   string
	isStop         bool
}

type deferredLegacyOutput struct {
	responseString string
	delta          string
}

func parseConversationEvent(line string, state *sseparser.PatchState, model string) (conversationStreamEvent, bool) {
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return conversationStreamEvent{}, false
	}

	if chunk, ok := sseparser.ChunkFromRaw(raw, model); ok {
		event := conversationStreamEvent{
			chunk:          &chunk,
			text:           sseparser.ChunkContent(chunk),
			role:           sseparser.ChunkRole(chunk),
			conversationID: chunk.ConversationID,
			channel:        sseparser.ChannelFromValue(raw),
			finishReason:   sseparser.ChunkFinishReason(chunk),
		}
		event.isStop = event.finishReason != ""
		return event, true
	}

	var direct chatgpt_types.ChatGPTResponse
	if err := json.Unmarshal([]byte(line), &direct); err == nil && sseparser.IsUsableConversationResponse(direct) {
		channel := sseparser.ChannelFromValue(raw)
		state.Channel = firstNonEmpty(channel, state.Channel)
		return conversationStreamEvent{response: direct, messageID: direct.Message.ID, channel: state.Channel}, true
	}

	if response, ok := sseparser.ResponseFromValue(raw["v"]); ok {
		if state.Response.Message.ID != "" && response.Message.ID != "" && response.Message.ID != state.Response.Message.ID {
			sseparser.ResetMessage(state)
		}
		state.Response = response
		if channel := sseparser.ChannelFromValue(raw["v"]); channel != "" {
			state.Channel = channel
		}
		return conversationStreamEvent{response: state.Response, messageID: state.Response.Message.ID, channel: state.Channel}, true
	}
	if text, ok := raw["v"].(string); ok && raw["p"] == nil && raw["o"] == nil {
		sseparser.EnsurePatchDefaults(state)
		current, _ := state.Response.Message.Content.Parts[0].(string)
		state.Response.Message.Content.Parts[0] = current + text
		return conversationStreamEvent{response: state.Response, messageID: state.Response.Message.ID, channel: state.Channel}, true
	}

	// Recent streams can send a bare patch array and omit the outer path.
	if batch, ok := raw["v"].([]interface{}); ok && raw["p"] == nil {
		applied := false
		sseparser.EnsurePatchDefaults(state)
		for _, item := range batch {
			op, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			subPath, _ := op["p"].(string)
			subOp, _ := op["o"].(string)
			if sseparser.ApplyPatch(state, subPath, subOp, op["v"]) {
				applied = true
			}
		}
		if applied {
			return conversationStreamEvent{response: state.Response, messageID: state.Response.Message.ID, channel: state.Channel}, true
		}
	}

	if patchPath, ok := raw["p"].(string); ok {
		patchOperation, _ := raw["o"].(string)
		if patchPath == "" && patchOperation == "patch" {
			if batch, ok := raw["v"].([]interface{}); ok {
				applied := false
				for _, item := range batch {
					op, ok := item.(map[string]interface{})
					if !ok {
						continue
					}
					subPath, _ := op["p"].(string)
					subOp, _ := op["o"].(string)
					if sseparser.ApplyPatch(state, subPath, subOp, op["v"]) {
						applied = true
					}
				}
				if applied {
					return conversationStreamEvent{response: state.Response, messageID: state.Response.Message.ID, channel: state.Channel}, true
				}
			}
		}
		if sseparser.ApplyPatch(state, patchPath, patchOperation, raw["v"]) {
			return conversationStreamEvent{response: state.Response, messageID: state.Response.Message.ID, channel: state.Channel}, true
		}
	}

	return conversationStreamEvent{}, false
}

// Handler 处理对话响应（简化版）。
func Handler(c *gin.Context, response *http.Response, client httpclient.AuroraHttpClient, account *accounts.Account, uuid string, translated_request chatgpt_types.ChatGPTRequest, stream bool, model string) (string, *ContinueInfo) {
	result := HandlerDetailed(c, response, client, account, uuid, translated_request, stream, model)
	return result.Text, result.Continue
}

// HandlerDetailed 处理对话响应（详细版）。
func HandlerDetailed(c *gin.Context, response *http.Response, client httpclient.AuroraHttpClient, account *accounts.Account, uuid string, translated_request chatgpt_types.ChatGPTRequest, stream bool, model string) HandlerResult {
	return HandlerDetailedWithWebsocket(c, response, client, account, uuid, translated_request, stream, model, nil)
}

// HandlerDetailedWithWebsocket 处理对话响应（带 WebSocket）。
func HandlerDetailedWithWebsocket(c *gin.Context, response *http.Response, client httpclient.AuroraHttpClient, account *accounts.Account, uuid string, translated_request chatgpt_types.ChatGPTRequest, stream bool, model string, wsConn *websocket.Conn) HandlerResult {
	return HandlerDetailedWithOptions(c, response, client, account, uuid, translated_request, stream, model, HandlerDetailedOptions{Websocket: wsConn})
}

// HandlerDetailedOptions 是 HandlerDetailedWithOptions 的可选参数。
type HandlerDetailedOptions struct {
	Websocket        *websocket.Conn
	ClientState      *ChatClientState
	ArtifactDelivery string
	ProxyURL         string
	Tools            []official_types.Tool
	// SuppressStreamOutput keeps parsing an upstream stream but leaves writing
	// to the caller. It is used by /v1/responses, whose SSE event shape differs
	// from Chat Completions.
	SuppressStreamOutput bool
	OnTextDelta          func(string)
	// OnThinkingDelta lets protocol adapters expose reasoning without changing
	// the Chat Completions stream shape.
	OnThinkingDelta func(string)
}

// HandlerDetailedWithOptions 处理对话响应流（最完整版）。
func HandlerDetailedWithOptions(c *gin.Context, response *http.Response, client httpclient.AuroraHttpClient, account *accounts.Account, uuid string, translated_request chatgpt_types.ChatGPTRequest, stream bool, model string, options HandlerDetailedOptions) HandlerResult {
	if model == "" {
		model = translated_request.Model
	}
	wsConn := options.Websocket
	if options.ClientState != nil {
		options.ClientState.ApplyToRequest(&translated_request)
	}
	max_tokens := false
	writeStream := stream && !options.SuppressStreamOutput

	reader := bufio.NewReader(response.Body)
	if wsConn != nil {
		// The orchestration layer may establish this connection for a
		// non-streaming extended/max request before posting the conversation.
		defer wsConn.Close()
	} else if stream && client != nil && account != nil {
		// Preserve the fallback for streaming callers that did not establish a
		// WebSocket before entering the response handler.
		if conn, err := DialChatWebsocketWithStateAndProxy(client, account, options.ClientState, options.ProxyURL); err == nil {
			wsConn = conn
			defer wsConn.Close()
		}
	}

	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
	} else {
		c.Header("Content-Type", "application/json")
	}
	var finish_reason string
	var previous_text typings.StringStruct
	var citePipeline sseparser.CiteStreamPipeline
	var visibleChunkText strings.Builder
	usedChunkEvents := false
	textSnapshots := make(map[string]*typings.StringStruct)
	// ChatGPT echoes the messages supplied in a stateless OpenAI request before
	// it streams the newly generated assistant message. Those assistant IDs are
	// input history, not output for this turn, and must never be forwarded to
	// the OpenAI client.
	historyAssistantMessageIDs := make(map[string]struct{})
	for _, message := range translated_request.Messages {
		if message.Author.Role == "assistant" {
			historyAssistantMessageIDs[message.ID.String()] = struct{}{}
		}
	}
	var original_response chatgpt_types.ChatGPTResponse
	var isRole = true
	var isEnd = false
	var imgSource []string
	var convId string
	var sentinel []map[string]interface{}
	var thinkingText string
	var emittedText string
	var activeChannel string
	var assistantMessageID string
	visibleText := func() string {
		if usedChunkEvents {
			return strings.Join(imgSource, "") + visibleChunkText.String()
		}
		return strings.Join(imgSource, "") + emittedText
	}
	artifactState := newArtifactAccumulator()
	artifactConfig := ArtifactStreamConfig{Delivery: options.ArtifactDelivery}
	var patchState sseparser.PatchState
	var handoffTopicID string
	var currentEvent string
	var readingWebsocket bool
	var websocketStream io.ReadCloser
	var deferredHTTPOutput []deferredLegacyOutput
	shouldDeferHTTPOutput := func() bool {
		return !readingWebsocket && handoffTopicID != "" && wsConn != nil
	}
	flushDeferredHTTPOutput := func() error {
		for _, output := range deferredHTTPOutput {
			if output.delta != "" {
				emittedText += output.delta
				if options.OnTextDelta != nil {
					options.OnTextDelta(output.delta)
				}
			}
			if writeStream {
				if _, err := writeLegacyResponseString(c, output.responseString); err != nil {
					return err
				}
			}
		}
		deferredHTTPOutput = nil
		return nil
	}
	emitSentinels := func(items []map[string]interface{}) {
		if len(items) == 0 {
			return
		}
		sentinel = append(sentinel, items...)
		if !writeStream {
			return
		}
		for _, item := range items {
			chunk := official_types.NewChatCompletionChunk("", model)
			if convId != "" {
				chunk.ConversationID = convId
			}
			chunk.Sentinel = item
			c.Writer.WriteString("data: " + chunk.String() + "\n\n")
			c.Writer.Flush()
		}
	}
	observeArtifacts := func(line string) {
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return
		}
		if cid := firstConversationID(raw); cid != "" && convId == "" {
			convId = cid
		}
		events := artifactState.ObserveRaw(raw, convId)
		emitSentinels(materializeArtifactEvents(client, account, convId, events, artifactConfig))
		if artifactState.LastAssistantMsgID != "" {
			assistantMessageID = artifactState.LastAssistantMsgID
		}
		if artifactState.ConversationID != "" && convId == "" {
			convId = artifactState.ConversationID
		}
	}
	emitThinking := func(delta string) {
		if delta == "" {
			return
		}
		thinkingText += delta
		if options.OnThinkingDelta != nil {
			options.OnThinkingDelta(delta)
		}
		emitSentinels([]map[string]interface{}{{
			"event": "thinking",
			"kind":  "analysis",
			"delta": delta,
		}})
		if writeStream {
			reasoningChunk := official_types.NewReasoningChunk(delta, model)
			if convId != "" {
				reasoningChunk.ConversationID = convId
			}
			c.Writer.WriteString("data: " + reasoningChunk.String() + "\n\n")
			c.Writer.Flush()
		}
	}
	finalizeArtifacts := func() {
		emitSentinels(materializeArtifactEvents(client, account, convId, artifactState.Finalize(), artifactConfig))
	}
	flushCites := func() {
		flushed := citePipeline.Flush(patchState.CiteAlts)
		if flushed == "" {
			return
		}
		if writeStream {
			chunk := official_types.NewChatCompletionChunk(flushed, model)
			chunk.ConversationID = convId
			_ = writeChatCompletionChunk(c, chunk)
		}
		if usedChunkEvents {
			visibleChunkText.WriteString(flushed)
		}
	}
readLoop:
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF && line == "" {
				break
			}
			if err != io.EOF {
				return HandlerResult{}
			}
		}
		if eventName, ok := sseparser.EventName(line); ok {
			currentEvent = eventName
		}
		for _, line := range sseparser.DataPayloads(line) {
			if strings.HasPrefix(line, "[DONE]") {
				if shouldUseWebsocketHandoff(readingWebsocket, handoffTopicID, wsConn, emittedText, imgSource) {
					wsReader, err := chatWebsocketStreamReader(wsConn, handoffTopicID)
					if err == nil {
						// The HTTP stream can be a partial transport while the topic
						// carries the complete assistant message. Drop the held partial
						// output and restart snapshot tracking for the handoff stream.
						deferredHTTPOutput = nil
						previous_text = typings.StringStruct{}
						textSnapshots = make(map[string]*typings.StringStruct)
						patchState = sseparser.PatchState{}
						isRole = true
						isEnd = false
						max_tokens = false
						finish_reason = ""
						websocketStream = wsReader
						defer websocketStream.Close()
						reader = bufio.NewReader(wsReader)
						readingWebsocket = true
						currentEvent = ""
						continue readLoop
					}
				}
				flushCites()
				if err := flushDeferredHTTPOutput(); err != nil {
					return HandlerResult{}
				}
				finalizeArtifacts()
				break readLoop
			}
			observeArtifacts(line)
			if topicID, skip := sseparser.HandoffTopicFromPayload(line, currentEvent); skip {
				if topicID != "" {
					handoffTopicID = topicID
				}
				currentEvent = ""
				continue
			}
			streamEvent, ok := parseConversationEvent(line, &patchState, model)
			if os.Getenv("DEBUG_SSE") != "" {
				debugText := streamEvent.text
				debugSrc := "chunk"
				if streamEvent.response.Message.ID != "" {
					debugText = sseparser.FirstStringPart(streamEvent.response.Message.Content.Parts)
					debugSrc = "response"
				}
				raw := strings.TrimSpace(line)
				if len(raw) > 200 {
					raw = raw[:200] + "..."
				}
				fmt.Printf("[sse-in] src=%s channel=%q textLen=%d finish=%q parsed=%v raw=%q\n", debugSrc, streamEvent.channel, len(debugText), streamEvent.finishReason, ok, raw)
			}
			if !ok {
				currentEvent = ""
				continue
			}
			if streamEvent.chunk != nil {
				usedChunkEvents = true
				if streamEvent.conversationID != "" {
					convId = streamEvent.conversationID
				}
				if streamEvent.chunk.Sentinel != nil {
					sentinel = append(sentinel, streamEvent.chunk.Sentinel)
				}
				rawDeltaText := sseparser.NormalizeContentDelta(previous_text.Text, streamEvent.text)
				deltaText := chatgpt.SanitizedContentDelta(previous_text.Text, rawDeltaText)
				deltaText = citePipeline.Feed(patchState.CiteAlts, deltaText)
				if streamEvent.channel != "" {
					activeChannel = streamEvent.channel
				}
				if streamEvent.finishReason != "" {
					finish_reason = streamEvent.finishReason
					if isTokenLimitFinish(finish_reason) {
						max_tokens = true
					}
					isEnd = true
				}
				if activeChannel == "analysis" {
					emitThinking(streamEvent.text)
					if streamEvent.isStop {
						willContinue := max_tokens && convId != "" && assistantMessageID != ""
						if writeStream && !willContinue {
							finalLine := official_types.StopChunkWithConversation(finish_reason, model, convId)
							c.Writer.WriteString("data: " + finalLine.String() + "\n\n")
							c.Writer.Flush()
						}
						if willContinue {
							finalizeArtifacts()
							return HandlerResult{
								Text:              visibleText(),
								ThinkingText:      thinkingText,
								ConversationID:    convId,
								ParentMessageID:   assistantMessageID,
								Sentinel:          sentinel,
								ArtifactSignals:   artifactState.Signals,
								SandboxArtifacts:  artifactState.SandboxArtifacts,
								PDFArtifacts:      artifactState.PDFArtifacts,
								GeneratedImageIDs: artifactState.ImageFileIDs,
								StopSent:          false,
								Continue: &ContinueInfo{
									ConversationID: convId,
									ParentID:       assistantMessageID,
								},
							}
						}
						finalizeArtifacts()
						return HandlerResult{
							Text:              visibleText(),
							ThinkingText:      thinkingText,
							ConversationID:    convId,
							ParentMessageID:   assistantMessageID,
							Sentinel:          sentinel,
							ArtifactSignals:   artifactState.Signals,
							SandboxArtifacts:  artifactState.SandboxArtifacts,
							PDFArtifacts:      artifactState.PDFArtifacts,
							GeneratedImageIDs: artifactState.ImageFileIDs,
							StopSent:          true,
						}
					}
					currentEvent = ""
					continue
				}
				if deltaText != "" && options.OnTextDelta != nil {
					options.OnTextDelta(deltaText)
				}
				if writeStream {
					outChunk := *streamEvent.chunk
					willContinue := streamEvent.isStop && max_tokens && convId != "" && assistantMessageID != ""
					if len(outChunk.Choices) > 0 {
						outChunk.Choices[0].Delta.Content = deltaText
						if willContinue {
							// A continuation follows immediately. Do not advertise a
							// terminal finish_reason to OpenAI clients yet, or they will
							// stop reading before the remaining text arrives.
							outChunk.Choices[0].FinishReason = nil
						}
						if streamEvent.role == "" || !isRole {
							outChunk.Choices[0].Delta.Role = ""
						} else {
							// Upstream image tools emit role="tool". That is an
							// internal producer role, not a valid streamed completion
							// role for OpenAI-compatible clients.
							outChunk.Choices[0].Delta.Role = "assistant"
						}
					}
					if streamEvent.isStop && outChunk.ConversationID == "" {
						outChunk.ConversationID = convId
					}
					shouldWrite := deltaText != "" ||
						(streamEvent.role != "" && isRole) ||
						streamEvent.chunk.Sentinel != nil ||
						(streamEvent.isStop && !willContinue)
					if shouldWrite {
						if err := writeChatCompletionChunk(c, outChunk); err != nil {
							return HandlerResult{}
						}
					}
					if streamEvent.role != "" && isRole {
						isRole = false
					}
				}
				if rawDeltaText != "" {
					previous_text.Text += rawDeltaText
				}
				if deltaText != "" {
					emittedText += deltaText
					visibleChunkText.WriteString(deltaText)
				}
				if streamEvent.isStop {
					flushCites()
					if max_tokens && convId != "" && assistantMessageID != "" {
						finalizeArtifacts()
						return HandlerResult{
							Text:              visibleText(),
							ThinkingText:      thinkingText,
							ConversationID:    convId,
							ParentMessageID:   assistantMessageID,
							Sentinel:          sentinel,
							ArtifactSignals:   artifactState.Signals,
							SandboxArtifacts:  artifactState.SandboxArtifacts,
							PDFArtifacts:      artifactState.PDFArtifacts,
							GeneratedImageIDs: artifactState.ImageFileIDs,
							StopSent:          false,
							Continue: &ContinueInfo{
								ConversationID: convId,
								ParentID:       assistantMessageID,
							},
						}
					}
					finalizeArtifacts()
					return HandlerResult{
						Text:              visibleText(),
						ThinkingText:      thinkingText,
						ConversationID:    convId,
						ParentMessageID:   assistantMessageID,
						Sentinel:          sentinel,
						ArtifactSignals:   artifactState.Signals,
						SandboxArtifacts:  artifactState.SandboxArtifacts,
						PDFArtifacts:      artifactState.PDFArtifacts,
						GeneratedImageIDs: artifactState.ImageFileIDs,
						StopSent:          true,
					}
				}
				currentEvent = ""
				continue
			}
			original_response = streamEvent.response
			if original_response.Error != nil {
				c.JSON(500, gin.H{"error": original_response.Error})
				return HandlerResult{}
			}
			sentinel = append(sentinel, sseparser.SentinelsFromResponse(original_response)...)
			if original_response.ConversationID != convId {
				if convId == "" {
					convId = original_response.ConversationID
				} else {
					continue
				}
			}
			if streamEvent.channel != "" {
				activeChannel = streamEvent.channel
			}
			if _, isHistory := historyAssistantMessageIDs[original_response.Message.ID]; isHistory {
				currentEvent = ""
				continue
			}
			if original_response.Message.ID != "" && (original_response.Message.Author.Role == "assistant" || original_response.Message.Author.Role == "tool") {
				assistantMessageID = original_response.Message.ID
			}
			if activeChannel == "analysis" {
				thinkingDelta := sseparser.NormalizeContentDelta(thinkingText, sseparser.FirstStringPart(original_response.Message.Content.Parts))
				emitThinking(thinkingDelta)
				currentEvent = ""
				continue
			}
			if !(original_response.Message.Author.Role == "assistant" || (original_response.Message.Author.Role == "tool" && original_response.Message.Content.ContentType != "text")) || original_response.Message.Content.Parts == nil {
				continue
			}
			// Rich-response streams frequently send the opening assistant snapshot
			// before attaching message_type/channel metadata. It is still visible
			// answer text; filtering it here made responses start at a random later
			// fragment once the metadata finally arrived. Analysis was handled above,
			// so forward every eligible assistant text message from this point on.
			if !strings.HasSuffix(original_response.Message.Content.ContentType, "text") {
				continue
			}
			if isEndTurn(original_response.Message.EndTurn) {
				isEnd = true
			}
			if len(original_response.Message.Metadata.Citations) != 0 {
				r := []rune(original_response.Message.Content.Parts[0].(string))
				offset := 0
				for _, citation := range original_response.Message.Metadata.Citations {
					rl := len(r)
					attr := urlAttrMap[citation.Metadata.URL]
					if attr == "" {
						u, _ := url.Parse(citation.Metadata.URL)
						BaseURL := u.Scheme + "://" + u.Host + "/"
						attr = getURLAttribution(client, account, BaseURL)
						if attr != "" {
							urlAttrMap[citation.Metadata.URL] = attr
						}
					}
					original_response.Message.Content.Parts[0] = string(r[:citation.StartIx+offset]) + " ([" + attr + "](" + citation.Metadata.URL + " \"" + citation.Metadata.Title + "\"))" + string(r[citation.EndIx+offset:])
					r = []rune(original_response.Message.Content.Parts[0].(string))
					offset += len(r) - rl
				}
			}
			response_string := ""
			textSnapshot := &previous_text
			if messageID := original_response.Message.ID; messageID != "" {
				textSnapshot = textSnapshots[messageID]
				if textSnapshot == nil {
					textSnapshot = &typings.StringStruct{}
					textSnapshots[messageID] = textSnapshot
				}
			}
			if original_response.Message.Recipient != "all" {
				continue
			}
			if original_response.Message.Content.ContentType == "multimodal_text" {
				apiUrl := BaseURL + "/files/"
				if FILES_REVERSE_PROXY != "" {
					apiUrl = FILES_REVERSE_PROXY
				}
				imgSource = make([]string, len(original_response.Message.Content.Parts))
				var wg sync.WaitGroup
				for index, part := range original_response.Message.Content.Parts {
					jsonItem, _ := json.Marshal(part)
					var dalle_content chatgpt_types.DalleContent
					err = json.Unmarshal(jsonItem, &dalle_content)
					if err != nil {
						continue
					}
					url := apiUrl + strings.Split(dalle_content.AssetPointer, "//")[1] + "/download"
					wg.Add(1)
					go GetImageSource(client, &wg, url, dalle_content.Metadata.Dalle.Prompt, account, index, imgSource)
				}
				wg.Wait()
				translated_response := official_types.NewChatCompletionChunk(strings.Join(imgSource, ""), model)
				if isRole {
					translated_response.Choices[0].Delta.Role = "assistant"
				}
				// A multimodal response can contain normal text before or after
				// image descriptors. Emit that text first so it is not discarded
				// while the renderer handles the image assets.
				if chatgpt.TextFromParts(original_response.Message.Content.Parts) != "" {
					response_string = chatgpt.ConvertToString(&original_response, textSnapshot, isRole, model)
				} else {
					response_string = "data: " + translated_response.String() + "\n\n"
				}
			}
			if response_string == "" {
				response_string = chatgpt.ConvertToString(&original_response, textSnapshot, isRole, model)
			}
			if response_string == "" {
				if isEnd {
					goto endProcess
				} else {
					continue
				}
			}
			if response_string == "【" {
				// The legacy web stream emits this as a source-loading sentinel.
				// It is not user-visible content; importantly, do not pause the
				// following text while waiting for citation metadata.
				continue
			}
		endProcess:
			isRole = false
			if original_response.Message.Metadata.FinishDetails != nil {
				finish_reason = original_response.Message.Metadata.FinishDetails.Type
				max_tokens = isTokenLimitFinish(finish_reason)
			}
			willContinue := isEnd && max_tokens && convId != "" && assistantMessageID != ""
			legacyDelta := chatCompletionDelta(response_string)
			if shouldDeferHTTPOutput() {
				deferredHTTPOutput = append(deferredHTTPOutput, deferredLegacyOutput{responseString: response_string, delta: legacyDelta})
			} else {
				if legacyDelta != "" {
					emittedText += legacyDelta
				}
				if options.OnTextDelta != nil && legacyDelta != "" {
					options.OnTextDelta(legacyDelta)
				}
				if writeStream {
					_, err = writeLegacyResponseString(c, response_string)
					if err != nil {
						return HandlerResult{}
					}
				}
			}

			if isEnd {
				flushCites()
				if shouldDeferHTTPOutput() {
					// Keep reading through [DONE] so the complete WebSocket handoff
					// can replace this partial HTTP response.
					currentEvent = ""
					continue
				}
				if writeStream && !willContinue {
					final_line := official_types.StopChunkWithConversation(finish_reason, model, convId)
					c.Writer.WriteString("data: " + final_line.String() + "\n\n")
					c.Writer.Flush()
				}
				finalizeArtifacts()
				if willContinue {
					return HandlerResult{
						Text:              visibleText(),
						ThinkingText:      thinkingText,
						ConversationID:    convId,
						ParentMessageID:   assistantMessageID,
						Sentinel:          sentinel,
						ArtifactSignals:   artifactState.Signals,
						SandboxArtifacts:  artifactState.SandboxArtifacts,
						PDFArtifacts:      artifactState.PDFArtifacts,
						GeneratedImageIDs: artifactState.ImageFileIDs,
						Continue: &ContinueInfo{
							ConversationID: convId,
							ParentID:       assistantMessageID,
						},
					}
				}
				return HandlerResult{
					Text:              visibleText(),
					ThinkingText:      thinkingText,
					ConversationID:    convId,
					ParentMessageID:   assistantMessageID,
					Sentinel:          sentinel,
					ArtifactSignals:   artifactState.Signals,
					SandboxArtifacts:  artifactState.SandboxArtifacts,
					PDFArtifacts:      artifactState.PDFArtifacts,
					GeneratedImageIDs: artifactState.ImageFileIDs,
					StopSent:          writeStream,
				}
			}
			currentEvent = ""
		}
		if err == io.EOF {
			flushCites()
			if flushErr := flushDeferredHTTPOutput(); flushErr != nil {
				return HandlerResult{}
			}
			break
		}
	}
	if !max_tokens {
		finalizeArtifacts()
		return HandlerResult{
			Text:              visibleText(),
			ThinkingText:      thinkingText,
			ConversationID:    convId,
			ParentMessageID:   assistantMessageID,
			Sentinel:          sentinel,
			ArtifactSignals:   artifactState.Signals,
			SandboxArtifacts:  artifactState.SandboxArtifacts,
			PDFArtifacts:      artifactState.PDFArtifacts,
			GeneratedImageIDs: artifactState.ImageFileIDs,
		}
	}
	finalizeArtifacts()
	return HandlerResult{
		Text:              visibleText(),
		ThinkingText:      thinkingText,
		ConversationID:    convId,
		ParentMessageID:   assistantMessageID,
		Sentinel:          sentinel,
		ArtifactSignals:   artifactState.Signals,
		SandboxArtifacts:  artifactState.SandboxArtifacts,
		PDFArtifacts:      artifactState.PDFArtifacts,
		GeneratedImageIDs: artifactState.ImageFileIDs,
		Continue: &ContinueInfo{
			ConversationID: original_response.ConversationID,
			ParentID:       original_response.Message.ID,
		},
	}
}

func isTokenLimitFinish(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max_completion_tokens", "token_limit":
		return true
	default:
		return false
	}
}

// isEndTurn only treats an explicit true as terminal. ChatGPT's upstream
// stream can include end_turn:false on intermediate snapshots; treating any
// non-nil value as terminal cuts the answer off at that snapshot.
func isEndTurn(value interface{}) bool {
	ended, ok := value.(bool)
	return ok && ended
}

// chatCompletionDelta extracts a text delta from the legacy stream format
// produced by ConvertToString. It intentionally ignores role-only and stop
// chunks, which do not map to a Responses output_text delta.
func chatCompletionDelta(responseString string) string {
	payload := strings.TrimSpace(strings.TrimPrefix(responseString, "data: "))
	var chunk official_types.ChatCompletionChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil || len(chunk.Choices) == 0 {
		return ""
	}
	return chunk.Choices[0].Delta.Content
}

// writeChatCompletionChunk keeps role and text in separate SSE events.  The
// OpenAI-compatible shape is a role-only event followed by text events. Some
// clients discard content when it shares the first event with `role`, which
// makes a rich response appear to start in the middle after its first snapshot.
func writeChatCompletionChunk(c *gin.Context, chunk official_types.ChatCompletionChunk) error {
	if len(chunk.Choices) > 0 {
		delta := chunk.Choices[0].Delta
		if delta.Role != "" && delta.Content != "" {
			roleChunk := chunk
			roleChunk.Choices = append([]official_types.Choices(nil), chunk.Choices...)
			roleChunk.Choices[0].Delta.Content = ""
			if _, err := c.Writer.WriteString("data: " + roleChunk.String() + "\n\n"); err != nil {
				return err
			}

			textChunk := chunk
			textChunk.Choices = append([]official_types.Choices(nil), chunk.Choices...)
			textChunk.Choices[0].Delta.Role = ""
			if _, err := c.Writer.WriteString("data: " + textChunk.String() + "\n\n"); err != nil {
				return err
			}
			c.Writer.Flush()
			return nil
		}
	}
	_, err := c.Writer.WriteString("data: " + chunk.String() + "\n\n")
	if err == nil {
		c.Writer.Flush()
	}
	return err
}

// writeLegacyResponseString writes a converted legacy SSE event.  ConvertToString
// returns an SSE string rather than a chunk, so decode it only when needed to
// apply the same role/text separation used for native OpenAI chunks.
func writeLegacyResponseString(c *gin.Context, responseString string) (int, error) {
	payload := strings.TrimSpace(strings.TrimPrefix(responseString, "data: "))
	var chunk official_types.ChatCompletionChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err == nil && len(chunk.Choices) > 0 {
		if chunk.Choices[0].Delta.Role != "" && chunk.Choices[0].Delta.Role != "assistant" {
			chunk.Choices[0].Delta.Role = "assistant"
			if err := writeChatCompletionChunk(c, chunk); err != nil {
				return 0, err
			}
			return len(responseString), nil
		}
		if chunk.Choices[0].Delta.Role != "" && chunk.Choices[0].Delta.Content != "" {
			if err := writeChatCompletionChunk(c, chunk); err != nil {
				return 0, err
			}
			return len(responseString), nil
		}
	}
	n, err := c.Writer.WriteString(responseString)
	if err == nil {
		c.Writer.Flush()
	}
	return n, err
}
