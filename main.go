package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/leokun/cursor-tab-server/internal/proto/agentv1"
	"github.com/leokun/cursor-tab-server/internal/proto/aiserverv1"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	defaultConfigPath       = "/app/config.yaml"
	defaultListenAddr       = ":443"
	headerCursorModelConfig = "X-Cursor-Model-Config"
	connectFlagCompressed   = 0x01

	exampleTokenPlaceholder = "REPLACE_WITH_REAL_CURSOR_SESSION_TOKEN"
)

var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"proxy-connection":    {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

var defaultUpstreamTargets = map[string]string{
	"/auth/full_stripe_profile":                             "https://api2.cursor.sh:443/auth/full_stripe_profile",
	"/aiserver.v1.AiService/StreamCpp":                      "https://api2.cursor.sh:443/aiserver.v1.AiService/StreamCpp",
	"/aiserver.v1.AiService/RefreshTabContext":              "https://api2.cursor.sh:443/aiserver.v1.AiService/RefreshTabContext",
	"/aiserver.v1.FileSyncService/FSSyncFile":               "https://api2.cursor.sh:443/aiserver.v1.FileSyncService/FSSyncFile",
	"/aiserver.v1.AiService/CppConfig":                      "https://api2.cursor.sh:443/aiserver.v1.AiService/CppConfig",
	"/aiserver.v1.FileSyncService/FSIsEnabledForUser":       "https://api2.cursor.sh:443/aiserver.v1.FileSyncService/FSIsEnabledForUser",
	"/aiserver.v1.CppService/RecordCppFate":                 "https://api2.cursor.sh:443/aiserver.v1.CppService/RecordCppFate",
	"/aiserver.v1.AiService/ReportAiCodeChangeMetrics":      "https://api2.cursor.sh:443/aiserver.v1.AiService/ReportAiCodeChangeMetrics",
	"/aiserver.v1.FileSyncService/FSConfig":                 "https://api2.cursor.sh:443/aiserver.v1.FileSyncService/FSConfig",
	"/aiserver.v1.FileSyncService/FSUploadFile":             "https://api2.cursor.sh:443/aiserver.v1.FileSyncService/FSUploadFile",
	"/aiserver.v1.BidiService/BidiAppend":                   "https://api2.cursor.sh:443/aiserver.v1.BidiService/BidiAppend",
	"/agent.v1.AgentService/RunSSE":                         "https://api2.cursor.sh:443/agent.v1.AgentService/RunSSE",
	"/aiserver.v1.DashboardService/GetEffectiveUserPlugins": "https://api2.cursor.sh:443/aiserver.v1.DashboardService/GetEffectiveUserPlugins",
}

type appConfig struct {
	Token string
}

type modelConfigHeader struct {
	BaseURL string `json:"baseURL"`
	APIKey  string `json:"apiKey"`
	ModelID string `json:"modelID"`
}

type serverApp struct {
	config          appConfig
	client          *http.Client
	upstreamTargets map[string]string
}

type unaryEnvelope struct {
	Message        proto.Message
	ConnectWrapped bool
	Compressed     bool
	GzipWrapped    bool
}

func main() {
	cfg, err := loadConfig(defaultConfigPath)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		os.Exit(1)
	}
	log.Printf("cursor-tab-server starting listen_addr=%s config_path=%s", defaultListenAddr, defaultConfigPath)
	server := &http.Server{
		Addr:              defaultListenAddr,
		Handler:           newServerApp(cfg, &http.Client{}, defaultUpstreamTargets),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		_, _ = fmt.Fprintf(os.Stderr, "listen failed: %v\n", err)
		os.Exit(1)
	}
}

func newServerApp(cfg appConfig, client *http.Client, upstreamTargets map[string]string) http.Handler {
	app := &serverApp{
		config:          cfg,
		client:          client,
		upstreamTargets: cloneUpstreamTargets(upstreamTargets),
	}
	if app.client == nil {
		app.client = &http.Client{}
	}
	return app
}

func (app *serverApp) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok"))
		return
	}
	if err := app.handleProxy(writer, request); err != nil {
		http.Error(writer, err.Error(), http.StatusBadGateway)
	}
}

func (app *serverApp) handleProxy(writer http.ResponseWriter, request *http.Request) error {
	if app == nil {
		return fmt.Errorf("server app is nil")
	}
	rawTarget, ok := app.upstreamTargets[strings.TrimSpace(request.URL.Path)]
	if !ok {
		http.NotFound(writer, request)
		return nil
	}
	targetURL, err := url.Parse(rawTarget)
	if err != nil {
		return fmt.Errorf("parse upstream url failed: %w", err)
	}
	targetURL.RawQuery = request.URL.RawQuery
	log.Printf("proxy request method=%s path=%s target=%s", request.Method, request.URL.Path, targetURL.String())

	requestBody := []byte{}
	if shouldRequestCarryBody(request.Method) {
		requestBody, err = io.ReadAll(request.Body)
		if err != nil {
			return fmt.Errorf("read request body failed: %w", err)
		}
	}
	if request.URL.Path == "/aiserver.v1.BidiService/BidiAppend" {
		requestBody, err = rewriteBidiAppendBody(requestBody, strings.TrimSpace(request.Header.Get("content-type")), strings.TrimSpace(request.Header.Get(headerCursorModelConfig)))
		if err != nil {
			log.Printf("proxy rewrite error path=%s err=%v", request.URL.Path, err)
			return err
		}
	}

	upstreamRequest, err := http.NewRequestWithContext(request.Context(), request.Method, targetURL.String(), bytes.NewReader(requestBody))
	if err != nil {
		return fmt.Errorf("build upstream request failed: %w", err)
	}
	copyRequestHeaders(upstreamRequest.Header, request.Header)
	upstreamRequest.Header.Del(headerCursorModelConfig)
	authorization := formatBearerAuthorization(app.config.Token)
	upstreamRequest.Header.Set("Authorization", authorization)
	upstreamRequest.Header.Set("x-cursor-checksum", buildCursorChecksum(authorization))
	if !shouldRequestCarryBody(request.Method) {
		upstreamRequest.Header.Del("content-length")
	} else {
		upstreamRequest.Header.Set("content-length", strconv.Itoa(len(requestBody)))
	}
	upstreamRequest.Host = targetURL.Host

	response, err := app.client.Do(upstreamRequest)
	if err != nil {
		log.Printf("proxy upstream error method=%s path=%s target=%s err=%v", request.Method, request.URL.Path, targetURL.String(), err)
		return fmt.Errorf("upstream request failed: %w", err)
	}
	defer response.Body.Close()
	log.Printf("proxy upstream response method=%s path=%s status=%d", request.Method, request.URL.Path, response.StatusCode)

	copyResponseHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	_, err = copyStream(writer, response.Body)
	return err
}

func loadConfig(path string) (appConfig, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return appConfig{}, err
	}
	token, err := parseTokenYAML(contents)
	if err != nil {
		return appConfig{}, err
	}
	return appConfig{Token: token}, nil
}

func parseTokenYAML(contents []byte) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "token" {
			continue
		}
		token := strings.TrimSpace(value)
		token = strings.TrimSuffix(token, "\r")
		if strings.HasPrefix(token, "\"") && strings.HasSuffix(token, "\"") {
			unquoted, err := strconv.Unquote(token)
			if err != nil {
				return "", fmt.Errorf("parse token failed: %w", err)
			}
			token = unquoted
		}
		if strings.HasPrefix(token, "'") && strings.HasSuffix(token, "'") && len(token) >= 2 {
			token = token[1 : len(token)-1]
		}
		token = strings.TrimSpace(token)
		switch token {
		case "":
			return "", fmt.Errorf("token is required")
		case exampleTokenPlaceholder:
			return "", fmt.Errorf("token placeholder must be replaced")
		}
		return token, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("token is required")
}

func rewriteBidiAppendBody(body []byte, contentType string, rawModelConfig string) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	headerValue := strings.TrimSpace(rawModelConfig)
	if headerValue == "" {
		return nil, fmt.Errorf("%s header is required", headerCursorModelConfig)
	}
	var config modelConfigHeader
	if err := json.Unmarshal([]byte(headerValue), &config); err != nil {
		return nil, fmt.Errorf("decode %s failed: %w", headerCursorModelConfig, err)
	}
	if strings.TrimSpace(config.APIKey) == "" || strings.TrimSpace(config.BaseURL) == "" || strings.TrimSpace(config.ModelID) == "" {
		return nil, fmt.Errorf("%s is incomplete", headerCursorModelConfig)
	}
	log.Printf("bidi_append rewrite requested model_id=%s base_url=%s", strings.TrimSpace(config.ModelID), strings.TrimSpace(config.BaseURL))

	request := &aiserverv1.BidiAppendRequest{}
	envelope, err := decodeUnaryEnvelope(body, contentType, request)
	if err != nil {
		return nil, fmt.Errorf("decode bidi append request failed: %w", err)
	}
	decodedRequest, _ := envelope.Message.(*aiserverv1.BidiAppendRequest)
	if decodedRequest == nil {
		return nil, fmt.Errorf("bidi append request missing payload")
	}
	message, _, err := decodeAgentClientMessage(decodedRequest.GetData())
	if err != nil {
		return nil, fmt.Errorf("decode agent client message failed: %w", err)
	}
	if message == nil {
		log.Printf("bidi_append passthrough append_seqno=%d reason=nil_agent_client_message", decodedRequest.GetAppendSeqno())
		return body, nil
	}
	rewrittenMessage, state := rewriteAgentClientMessage(message, config)
	if !state.changed {
		log.Printf("bidi_append passthrough append_seqno=%d reason=%s", decodedRequest.GetAppendSeqno(), state.reason)
		return body, nil
	}
	payload, err := proto.Marshal(rewrittenMessage)
	if err != nil {
		return nil, fmt.Errorf("encode agent client message failed: %w", err)
	}
	decodedRequest.Data = hex.EncodeToString(payload)
	runLikeJSON, jsonErr := marshalRunLikeJSON(rewrittenMessage)
	if jsonErr != nil {
		log.Printf("bidi_append rewritten append_seqno=%d run_like_json_error=%v", decodedRequest.GetAppendSeqno(), jsonErr)
	} else {
		log.Printf("bidi_append rewritten run_like append_seqno=%d kind=%s json=%s", decodedRequest.GetAppendSeqno(), state.kind, runLikeJSON)
	}
	log.Printf("bidi_append rewritten append_seqno=%d kind=%s model_id=%s max_mode=%t", decodedRequest.GetAppendSeqno(), state.kind, state.modelID, state.maxMode)
	return encodeUnaryEnvelope(decodedRequest, envelope)
}

type rewriteState struct {
	changed bool
	reason  string
	kind    string
	modelID string
	maxMode bool
}

func rewriteAgentClientMessage(message *agentv1.AgentClientMessage, config modelConfigHeader) (*agentv1.AgentClientMessage, rewriteState) {
	if message == nil {
		return nil, rewriteState{reason: "nil_agent_client_message"}
	}
	if runRequest := message.GetRunRequest(); runRequest != nil {
		rewrittenRun, state := rewriteRunLikeRequest("run_request", runRequest.GetModelDetails(), runRequest.GetRequestedModel(), config)
		if !state.changed {
			return message, state
		}
		clone := proto.Clone(message).(*agentv1.AgentClientMessage)
		clone.GetRunRequest().ModelDetails = rewrittenRun.modelDetails
		clone.GetRunRequest().RequestedModel = rewrittenRun.requestedModel
		return clone, state
	}
	if prewarmRequest := message.GetPrewarmRequest(); prewarmRequest != nil {
		rewrittenPrewarm, state := rewriteRunLikeRequest("prewarm_request", prewarmRequest.GetModelDetails(), prewarmRequest.GetRequestedModel(), config)
		if !state.changed {
			return message, state
		}
		clone := proto.Clone(message).(*agentv1.AgentClientMessage)
		clone.GetPrewarmRequest().ModelDetails = rewrittenPrewarm.modelDetails
		clone.GetPrewarmRequest().RequestedModel = rewrittenPrewarm.requestedModel
		return clone, state
	}
	return message, rewriteState{reason: "no_run_like_request"}
}

type rewrittenRunLike struct {
	modelDetails   *agentv1.ModelDetails
	requestedModel *agentv1.RequestedModel
}

func rewriteRunLikeRequest(kind string, modelDetails *agentv1.ModelDetails, requestedModel *agentv1.RequestedModel, config modelConfigHeader) (rewrittenRunLike, rewriteState) {
	if hasCustomProviderModelDetails(modelDetails) || hasCustomProviderRequestedModel(requestedModel) {
		return rewrittenRunLike{modelDetails: modelDetails, requestedModel: requestedModel}, rewriteState{
			reason: "custom_provider_detected",
			kind:   kind,
		}
	}

	nextModelDetails, modelChanged, maxMode := rewriteModelDetailsNode(modelDetails, config)
	nextRequestedModel, requestedChanged := rewriteRequestedModelNode(requestedModel, config)
	if !modelChanged && !requestedChanged {
		return rewrittenRunLike{modelDetails: modelDetails, requestedModel: requestedModel}, rewriteState{
			reason: "run_like_no_change",
			kind:   kind,
		}
	}

	return rewrittenRunLike{
			modelDetails:   nextModelDetails,
			requestedModel: nextRequestedModel,
		}, rewriteState{
			changed: true,
			reason:  "rewritten",
			kind:    kind,
			modelID: strings.TrimSpace(config.ModelID),
			maxMode: maxMode,
		}
}

func rewriteModelDetailsNode(node *agentv1.ModelDetails, config modelConfigHeader) (*agentv1.ModelDetails, bool, bool) {
	maxMode := false
	if node != nil {
		maxMode = node.GetMaxMode()
	}
	rewritten := &agentv1.ModelDetails{
		ModelId: strings.TrimSpace(config.ModelID),
		MaxMode: boolPtr(maxMode),
		Credentials: &agentv1.ModelDetails_ApiKeyCredentials{
			ApiKeyCredentials: &agentv1.ApiKeyCredentials{
				ApiKey:  strings.TrimSpace(config.APIKey),
				BaseUrl: stringPtr(strings.TrimSpace(config.BaseURL)),
			},
		},
	}
	if node == nil {
		return rewritten, true, maxMode
	}
	changed := node.GetModelId() != rewritten.GetModelId() ||
		node.GetMaxMode() != rewritten.GetMaxMode() ||
		node.GetApiKeyCredentials() == nil ||
		node.GetApiKeyCredentials().GetApiKey() != rewritten.GetApiKeyCredentials().GetApiKey() ||
		node.GetApiKeyCredentials().GetBaseUrl() != rewritten.GetApiKeyCredentials().GetBaseUrl() ||
		node.GetAzureCredentials() != nil ||
		node.GetBedrockCredentials() != nil ||
		node.GetDisplayModelId() != "" ||
		node.GetDisplayName() != "" ||
		node.GetDisplayNameShort() != "" ||
		len(node.GetAliases()) > 0 ||
		node.GetThinkingDetails() != nil
	return rewritten, changed, maxMode
}

func rewriteRequestedModelNode(node *agentv1.RequestedModel, config modelConfigHeader) (*agentv1.RequestedModel, bool) {
	if node == nil {
		return nil, false
	}
	rewritten := &agentv1.RequestedModel{
		ModelId:    strings.TrimSpace(config.ModelID),
		MaxMode:    node.GetMaxMode(),
		Parameters: cloneRequestedModelParameters(node.GetParameters()),
		Credentials: &agentv1.RequestedModel_ApiKeyCredentials{
			ApiKeyCredentials: &agentv1.ApiKeyCredentials{
				ApiKey:  strings.TrimSpace(config.APIKey),
				BaseUrl: stringPtr(strings.TrimSpace(config.BaseURL)),
			},
		},
	}
	changed := node.GetModelId() != rewritten.GetModelId() ||
		node.GetMaxMode() != rewritten.GetMaxMode() ||
		node.GetApiKeyCredentials() == nil ||
		node.GetApiKeyCredentials().GetApiKey() != rewritten.GetApiKeyCredentials().GetApiKey() ||
		node.GetApiKeyCredentials().GetBaseUrl() != rewritten.GetApiKeyCredentials().GetBaseUrl() ||
		node.GetAzureCredentials() != nil ||
		node.GetBedrockCredentials() != nil
	return rewritten, changed
}

func cloneRequestedModelParameters(input []*agentv1.RequestedModel_ModelParameterValue) []*agentv1.RequestedModel_ModelParameterValue {
	if len(input) == 0 {
		return nil
	}
	output := make([]*agentv1.RequestedModel_ModelParameterValue, 0, len(input))
	for _, item := range input {
		if item == nil {
			output = append(output, nil)
			continue
		}
		output = append(output, proto.Clone(item).(*agentv1.RequestedModel_ModelParameterValue))
	}
	return output
}

func hasCustomProviderModelDetails(message *agentv1.ModelDetails) bool {
	return message != nil && message.GetApiKeyCredentials() != nil && strings.TrimSpace(message.GetApiKeyCredentials().GetBaseUrl()) != ""
}

func hasCustomProviderRequestedModel(message *agentv1.RequestedModel) bool {
	return message != nil && message.GetApiKeyCredentials() != nil && strings.TrimSpace(message.GetApiKeyCredentials().GetBaseUrl()) != ""
}

func marshalRunLikeJSON(message *agentv1.AgentClientMessage) (string, error) {
	if message == nil {
		return "", nil
	}
	var payload proto.Message
	switch {
	case message.GetRunRequest() != nil:
		payload = message.GetRunRequest()
	case message.GetPrewarmRequest() != nil:
		payload = message.GetPrewarmRequest()
	default:
		return "", nil
	}
	data, err := protojson.MarshalOptions{
		UseProtoNames:   false,
		EmitUnpopulated: false,
	}.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func decodeAgentClientMessage(hexData string) (*agentv1.AgentClientMessage, string, error) {
	trimmed := strings.TrimSpace(hexData)
	if trimmed == "" {
		return nil, "", nil
	}
	payload, err := hex.DecodeString(trimmed)
	if err != nil {
		return nil, "", fmt.Errorf("bidi append data is not valid hex: %w", err)
	}
	clientMessage := &agentv1.AgentClientMessage{}
	if err := proto.Unmarshal(payload, clientMessage); err != nil {
		return nil, "", fmt.Errorf("decode agent client message failed: %w", err)
	}
	return clientMessage, detectClientMessageKind(clientMessage), nil
}

func detectClientMessageKind(message *agentv1.AgentClientMessage) string {
	if message == nil || message.GetMessage() == nil {
		return ""
	}
	switch message.GetMessage().(type) {
	case *agentv1.AgentClientMessage_RunRequest:
		return "run_request"
	case *agentv1.AgentClientMessage_PrewarmRequest:
		return "prewarm_request"
	default:
		return ""
	}
}

func decodeUnaryEnvelope(body []byte, contentType string, message proto.Message) (*unaryEnvelope, error) {
	if message == nil {
		return nil, errors.New("message is required")
	}
	payload := append([]byte(nil), body...)
	envelope := &unaryEnvelope{}
	if isGzipWrapped(payload) {
		reader, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		payload, err = io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		envelope.GzipWrapped = true
	}
	if shouldUseConnectEnvelope(contentType) {
		connectEnvelope, err := decodeConnectUnaryEnvelope(payload, message)
		if err == nil {
			connectEnvelope.GzipWrapped = envelope.GzipWrapped
			return connectEnvelope, nil
		}
		rawEnvelope, rawErr := decodeRawUnaryPayload(payload, message)
		if rawErr == nil {
			rawEnvelope.GzipWrapped = envelope.GzipWrapped
			return rawEnvelope, nil
		}
		return nil, err
	}
	rawEnvelope, rawErr := decodeRawUnaryPayload(payload, message)
	if rawErr == nil {
		rawEnvelope.GzipWrapped = envelope.GzipWrapped
		return rawEnvelope, nil
	}
	connectEnvelope, connectErr := decodeConnectUnaryEnvelope(payload, message)
	if connectErr == nil {
		connectEnvelope.GzipWrapped = envelope.GzipWrapped
		return connectEnvelope, nil
	}
	return nil, rawErr
}

func decodeRawUnaryPayload(body []byte, message proto.Message) (*unaryEnvelope, error) {
	decoded := cloneProtoMessage(message)
	if err := proto.Unmarshal(body, decoded); err != nil {
		return nil, err
	}
	return &unaryEnvelope{Message: decoded}, nil
}

func decodeConnectUnaryEnvelope(body []byte, message proto.Message) (*unaryEnvelope, error) {
	if len(body) < 5 {
		return nil, errors.New("connect envelope is truncated")
	}
	flags := body[0]
	payloadLength := int(binary.BigEndian.Uint32(body[1:5]))
	if payloadLength < 0 || len(body) != 5+payloadLength {
		return nil, errors.New("connect envelope payload is truncated")
	}
	payload := body[5:]
	if flags&connectFlagCompressed != 0 {
		reader, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		payload, err = io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
	}
	decoded := cloneProtoMessage(message)
	if err := proto.Unmarshal(payload, decoded); err != nil {
		return nil, err
	}
	return &unaryEnvelope{
		Message:        decoded,
		ConnectWrapped: true,
		Compressed:     flags&connectFlagCompressed != 0,
	}, nil
}

func encodeUnaryEnvelope(message proto.Message, envelope *unaryEnvelope) ([]byte, error) {
	if message == nil {
		return nil, errors.New("message is required")
	}
	if envelope == nil {
		envelope = &unaryEnvelope{}
	}
	payload, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}
	body := payload
	if envelope.ConnectWrapped {
		flags := byte(0)
		if envelope.Compressed {
			flags |= connectFlagCompressed
			body, err = gzipBytes(payload)
			if err != nil {
				return nil, err
			}
		}
		frame := make([]byte, 5+len(body))
		frame[0] = flags
		binary.BigEndian.PutUint32(frame[1:5], uint32(len(body)))
		copy(frame[5:], body)
		body = frame
	}
	if envelope.GzipWrapped {
		return gzipBytes(body)
	}
	return body, nil
}

func copyRequestHeaders(target http.Header, source http.Header) {
	for key, values := range source {
		lowerKey := strings.ToLower(key)
		if _, exists := hopByHopHeaders[lowerKey]; exists {
			continue
		}
		for _, value := range values {
			target.Add(key, value)
		}
	}
}

func copyResponseHeaders(target http.Header, source http.Header) {
	for key, values := range source {
		lowerKey := strings.ToLower(key)
		if _, exists := hopByHopHeaders[lowerKey]; exists {
			continue
		}
		for _, value := range values {
			target.Add(key, value)
		}
	}
}

func copyStream(writer io.Writer, reader io.Reader) (int64, error) {
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		readCount, readErr := reader.Read(buffer)
		if readCount > 0 {
			chunk := buffer[:readCount]
			written, writeErr := writer.Write(chunk)
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written < len(chunk) {
				return total, io.ErrShortWrite
			}
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

func buildCursorChecksum(authorization string) string {
	const (
		checksumTimestampDivisor = 1_000_000
		checksumInitialSeed      = 165
	)
	timestamp := time.Now().UnixMilli() / checksumTimestampDivisor
	timestampBytes := make([]byte, 6)
	timestampBigInt := big.NewInt(timestamp)
	for index := 0; index < len(timestampBytes); index++ {
		shift := uint((len(timestampBytes) - 1 - index) * 8)
		timestampBytes[index] = byte(new(big.Int).Rsh(timestampBigInt, shift).Uint64() & 0xff)
	}
	seed := checksumInitialSeed
	for index := 0; index < len(timestampBytes); index++ {
		current := int(timestampBytes[index]^byte(seed)) + (index % 256)
		current &= 0xff
		timestampBytes[index] = byte(current)
		seed = current
	}
	prefix := strings.TrimRight(base64.StdEncoding.EncodeToString(timestampBytes), "=")
	hashBytes := sha256.Sum256([]byte(strings.TrimSpace(authorization)))
	hash := fmt.Sprintf("%x", hashBytes)
	return prefix + hash[:32]
}

func shouldRequestCarryBody(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet, http.MethodHead, http.MethodDelete:
		return false
	default:
		return true
	}
}

func formatBearerAuthorization(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		return value
	}
	return "Bearer " + value
}

func shouldUseConnectEnvelope(contentType string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(contentType)), "connect")
}

func cloneProtoMessage(message proto.Message) proto.Message {
	if message == nil {
		return nil
	}
	return message.ProtoReflect().New().Interface()
}

func isGzipWrapped(body []byte) bool {
	return len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b
}

func gzipBytes(payload []byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(payload); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func boolPtr(value bool) *bool {
	return &value
}

func stringPtr(value string) *string {
	return &value
}

func cloneUpstreamTargets(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
