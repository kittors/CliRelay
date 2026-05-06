package multimodaladapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type Route struct {
	RequestedModel   string
	UpstreamProvider string
	UpstreamModel    string
	Protocol         string
}

type Report struct {
	Applied    bool
	MediaItems int
	Extractor  string
	Injected   bool
	Stripped   bool
}

type StatusError struct {
	StatusCodeValue int
	Message         string
	Code            string
}

func (e StatusError) Error() string {
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = http.StatusText(e.StatusCode())
	}
	code := strings.TrimSpace(e.Code)
	if code == "" {
		code = "multimodal_adapter_unavailable"
	}
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
	if err != nil {
		return message
	}
	return string(body)
}

func (e StatusError) StatusCode() int {
	if e.StatusCodeValue > 0 {
		return e.StatusCodeValue
	}
	return http.StatusInternalServerError
}

type mediaRef struct {
	Kind string `json:"type"`
	URL  string `json:"url"`
}

type selectedRule struct {
	action            string
	unavailableAction string
	injectAs          string
	maxMediaItems     int
	maxOutputBytes    int
	extractor         config.MultimodalExtractorConfig
	extractorName     string
}

// Apply converts media inputs into textual context after the concrete upstream route is known.
func Apply(ctx context.Context, raw []byte, route Route, cfg config.MultimodalAdaptersConfig) ([]byte, Report, error) {
	var report Report
	if !enabled(cfg) || len(raw) == 0 {
		return raw, report, nil
	}
	rule, ok := selectRule(cfg, route)
	if !ok {
		return raw, report, nil
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return raw, report, nil
	}
	refs := collectMediaRefs(payload)
	if len(refs) == 0 {
		return raw, report, nil
	}
	if rule.maxMediaItems > 0 && len(refs) > rule.maxMediaItems {
		refs = refs[:rule.maxMediaItems]
	}
	report.MediaItems = len(refs)
	report.Extractor = rule.extractorName

	switch rule.action {
	case "reject":
		return raw, report, StatusError{StatusCodeValue: http.StatusBadRequest, Message: "request contains media inputs but the selected upstream route is configured to reject media", Code: "multimodal_media_rejected"}
	case "strip":
		return stripAndInject(raw, payload, route.Protocol, rule, "Media inputs were removed because the selected upstream model does not support direct multimodal input.", &report)
	}

	if rule.extractor.Name == "" {
		return unavailable(raw, payload, route.Protocol, rule, "multimodal adapter matched this route but no extractor is configured", &report)
	}
	summaries, err := extractVisualContext(ctx, refs, route, rule.extractor)
	if err != nil {
		return unavailable(raw, payload, route.Protocol, rule, err.Error(), &report)
	}
	summary := strings.TrimSpace(strings.Join(summaries, "\n\n"))
	if summary == "" {
		return unavailable(raw, payload, route.Protocol, rule, "multimodal adapter returned empty visual context", &report)
	}
	return stripAndInject(raw, payload, route.Protocol, rule, limitStringBytes(summary, rule.maxOutputBytes), &report)
}

func enabled(cfg config.MultimodalAdaptersConfig) bool {
	return cfg.Enabled == nil || *cfg.Enabled
}

func selectRule(cfg config.MultimodalAdaptersConfig, route Route) (selectedRule, bool) {
	extractors := map[string]config.MultimodalExtractorConfig{}
	for _, extractor := range cfg.Extractors {
		extractors[strings.ToLower(strings.TrimSpace(extractor.Name))] = extractor
	}
	for _, rule := range cfg.Rules {
		if !matchRoute(rule.Match, route) {
			continue
		}
		action := strings.ToLower(strings.TrimSpace(rule.Action))
		if action == "" {
			action = strings.ToLower(strings.TrimSpace(cfg.DefaultAction))
		}
		if action == "" {
			action = "extract"
		}
		unavailable := strings.ToLower(strings.TrimSpace(rule.UnavailableAction))
		if unavailable == "" {
			unavailable = strings.ToLower(strings.TrimSpace(cfg.UnavailableAction))
		}
		if unavailable == "" {
			unavailable = "reject"
		}
		injectAs := strings.TrimSpace(rule.InjectAs)
		if injectAs == "" {
			injectAs = strings.TrimSpace(cfg.InjectAs)
		}
		if injectAs == "" {
			injectAs = "visual_context"
		}
		maxMediaItems := rule.MaxMediaItems
		if maxMediaItems <= 0 {
			maxMediaItems = cfg.MaxMediaItems
		}
		if maxMediaItems <= 0 {
			maxMediaItems = 4
		}
		maxOutputBytes := rule.MaxOutputBytes
		if maxOutputBytes <= 0 {
			maxOutputBytes = cfg.MaxOutputBytes
		}
		if maxOutputBytes <= 0 {
			maxOutputBytes = 12000
		}
		extractorName := strings.TrimSpace(rule.Extractor)
		var extractor config.MultimodalExtractorConfig
		if extractorName != "" {
			extractor = extractors[strings.ToLower(extractorName)]
		}
		return selectedRule{
			action:            normalizeAction(action),
			unavailableAction: normalizeUnavailableAction(unavailable),
			injectAs:          injectAs,
			maxMediaItems:     maxMediaItems,
			maxOutputBytes:    maxOutputBytes,
			extractor:         extractor,
			extractorName:     extractorName,
		}, true
	}
	return selectedRule{}, false
}

func normalizeAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "reject":
		return "reject"
	case "strip":
		return "strip"
	default:
		return "extract"
	}
}

func normalizeUnavailableAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "strip":
		return "strip"
	case "pass-through":
		return "pass-through"
	default:
		return "reject"
	}
}

func matchRoute(match config.MultimodalAdapterMatch, route Route) bool {
	return listMatches(match.RequestedModels, route.RequestedModel, false) &&
		listMatches(match.UpstreamProviders, route.UpstreamProvider, true) &&
		listMatches(match.UpstreamModels, route.UpstreamModel, false) &&
		listMatches(match.Protocols, route.Protocol, true)
}

func listMatches(values []string, candidate string, lower bool) bool {
	if len(values) == 0 {
		return true
	}
	candidate = strings.TrimSpace(candidate)
	if lower {
		candidate = strings.ToLower(candidate)
	}
	for _, value := range values {
		v := strings.TrimSpace(value)
		if lower {
			v = strings.ToLower(v)
		}
		if strings.EqualFold(v, candidate) {
			return true
		}
	}
	return false
}

func unavailable(raw []byte, payload any, protocol string, rule selectedRule, message string, report *Report) ([]byte, Report, error) {
	switch rule.unavailableAction {
	case "pass-through":
		return raw, *report, nil
	case "strip":
		return stripAndInject(raw, payload, protocol, rule, "Media inputs were removed because visual extraction is unavailable: "+message, report)
	default:
		return raw, *report, StatusError{StatusCodeValue: http.StatusBadRequest, Message: message, Code: "multimodal_extractor_unavailable"}
	}
}

func stripAndInject(raw []byte, payload any, protocol string, rule selectedRule, text string, report *Report) ([]byte, Report, error) {
	_ = raw
	stripped := stripMedia(payload)
	out, err := injectVisualContext(stripped, protocol, rule.injectAs, text)
	if err != nil {
		return nil, *report, err
	}
	report.Applied = true
	report.Stripped = true
	report.Injected = true
	return out, *report, nil
}

func collectMediaRefs(value any) []mediaRef {
	var refs []mediaRef
	var walk func(any)
	walk = func(v any) {
		switch typed := v.(type) {
		case map[string]any:
			kind := mediaKindFromMap(typed)
			if kind == "image" {
				if ref := mediaURLFromValue(typed["image_url"]); ref != "" {
					refs = append(refs, mediaRef{Kind: "image", URL: ref})
				}
			}
			if kind == "file" {
				if ref := mediaURLFromValue(typed["file_url"]); ref != "" {
					refs = append(refs, mediaRef{Kind: "file", URL: ref})
				}
			}
			if kind == "video" {
				if ref := mediaURLFromValue(typed["video_url"]); ref != "" {
					refs = append(refs, mediaRef{Kind: "video", URL: ref})
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return dedupeRefs(refs)
}

func mediaKindFromMap(m map[string]any) string {
	if rawType, ok := m["type"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(rawType)) {
		case "input_image", "image":
			return "image"
		case "input_file", "file":
			return "file"
		case "input_video", "video":
			return "video"
		}
	}
	if _, ok := m["image_url"]; ok {
		return "image"
	}
	if _, ok := m["file_url"]; ok {
		return "file"
	}
	if _, ok := m["video_url"]; ok {
		return "video"
	}
	return ""
}

func mediaURLFromValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		for _, key := range []string{"url", "uri", "path", "file_url", "image_url"} {
			if v, ok := typed[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func dedupeRefs(refs []mediaRef) []mediaRef {
	out := make([]mediaRef, 0, len(refs))
	seen := map[string]struct{}{}
	for _, ref := range refs {
		key := ref.Kind + "\x00" + ref.URL
		if strings.TrimSpace(ref.URL) == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ref)
	}
	return out
}

func stripMedia(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		if mediaKindFromMap(typed) != "" {
			return nil
		}
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if isMediaKey(key) {
				continue
			}
			stripped := stripMedia(child)
			if stripped != nil {
				out[key] = stripped
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, child := range typed {
			stripped := stripMedia(child)
			if stripped != nil {
				out = append(out, stripped)
			}
		}
		return out
	default:
		return value
	}
}

func isMediaKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "image_url", "file_url", "video_url":
		return true
	default:
		return false
	}
}

func injectVisualContext(payload any, protocol, injectAs, contextText string) ([]byte, error) {
	root, ok := payload.(map[string]any)
	if !ok {
		return json.Marshal(payload)
	}
	text := strings.TrimSpace(contextText)
	if text == "" {
		return json.Marshal(payload)
	}
	label := strings.TrimSpace(injectAs)
	if label == "" {
		label = "visual_context"
	}
	if strings.EqualFold(protocol, "openai-response") {
		item := map[string]any{
			"type": "message",
			"role": "developer",
			"content": []any{
				map[string]any{"type": "input_text", "text": "[" + label + "]\n" + text},
			},
		}
		items, _ := root["input"].([]any)
		root["input"] = append([]any{item}, items...)
		return json.Marshal(root)
	}
	item := map[string]any{"role": "system", "content": "[" + label + "]\n" + text}
	items, _ := root["messages"].([]any)
	root["messages"] = append([]any{item}, items...)
	return json.Marshal(root)
}

func extractVisualContext(ctx context.Context, refs []mediaRef, route Route, extractor config.MultimodalExtractorConfig) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(extractor.Type)) {
	case "http":
		return callHTTPExtractor(ctx, refs, route, extractor)
	case "zai-vision-http":
		return callZAIVisionHTTPExtractor(ctx, refs, extractor)
	case "mcp":
		return callMCPExtractor(ctx, refs, extractor)
	default:
		return nil, fmt.Errorf("multimodal adapter extractor %q has unsupported type %q", extractor.Name, extractor.Type)
	}
}

func callHTTPExtractor(ctx context.Context, refs []mediaRef, route Route, extractor config.MultimodalExtractorConfig) ([]string, error) {
	if strings.TrimSpace(extractor.Endpoint) == "" {
		return nil, fmt.Errorf("multimodal adapter extractor %q endpoint is not configured", extractor.Name)
	}
	timeout := time.Duration(extractor.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{
		"media":             refs,
		"prompt":            extractor.Prompt,
		"requested_model":   route.RequestedModel,
		"upstream_provider": route.UpstreamProvider,
		"upstream_model":    route.UpstreamModel,
		"protocol":          route.Protocol,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, os.ExpandEnv(extractor.Endpoint), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range extractor.Headers {
		if strings.TrimSpace(key) != "" {
			req.Header.Set(key, os.ExpandEnv(value))
		}
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("multimodal adapter extractor %q request failed: %w", extractor.Name, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("multimodal adapter extractor %q returned status %d: %s", extractor.Name, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	text := extractHTTPText(data)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("multimodal adapter extractor %q returned no text", extractor.Name)
	}
	return []string{text}, nil
}

func callZAIVisionHTTPExtractor(ctx context.Context, refs []mediaRef, extractor config.MultimodalExtractorConfig) ([]string, error) {
	if strings.TrimSpace(extractor.Endpoint) == "" {
		return nil, fmt.Errorf("multimodal adapter extractor %q endpoint is not configured", extractor.Name)
	}
	imageRefs := make([]mediaRef, 0, len(refs))
	for _, ref := range refs {
		if ref.Kind == "image" && strings.TrimSpace(ref.URL) != "" {
			imageRefs = append(imageRefs, ref)
		}
	}
	if len(imageRefs) == 0 {
		return nil, fmt.Errorf("multimodal adapter extractor %q requires at least one image input", extractor.Name)
	}
	timeout := time.Duration(extractor.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	prompt := strings.TrimSpace(extractor.Prompt)
	if prompt == "" {
		prompt = "Describe this visual input for a coding assistant. Extract visible text, UI elements, errors, filenames, paths, code snippets, charts, and anything needed to answer the user's request. Be concise and factual."
	}
	content := []map[string]any{{"type": "text", "text": prompt}}
	for _, ref := range imageRefs {
		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": strings.TrimSpace(ref.URL),
			},
		})
	}
	model := strings.TrimSpace(extractor.ToolName)
	if model == "" {
		model = strings.TrimSpace(extractor.Env["model"])
	}
	if model == "" {
		model = "glm-5.1"
	}
	maxTokens := 512
	if rawMaxTokens := strings.TrimSpace(extractor.Env["max_tokens"]); rawMaxTokens != "" {
		if parsed, err := strconv.Atoi(rawMaxTokens); err == nil && parsed > 0 {
			maxTokens = parsed
		}
	}
	requestBody := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": content,
			},
		},
		"max_tokens": maxTokens,
	}
	if !strings.EqualFold(strings.TrimSpace(extractor.Env["disable_thinking"]), "false") {
		requestBody["thinking"] = map[string]any{"type": "disabled"}
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(os.ExpandEnv(extractor.Endpoint), "/")
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range extractor.Headers {
		if strings.TrimSpace(key) != "" {
			req.Header.Set(key, os.ExpandEnv(value))
		}
	}
	if req.Header.Get("Authorization") == "" {
		if apiKey := strings.TrimSpace(os.ExpandEnv(extractor.Env["api_key"])); apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("multimodal adapter extractor %q request failed: %w", extractor.Name, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("multimodal adapter extractor %q returned status %d: %s", extractor.Name, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	text := extractHTTPText(data)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("multimodal adapter extractor %q returned no text", extractor.Name)
	}
	return []string{text}, nil
}

func extractHTTPText(data []byte) string {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return strings.TrimSpace(string(data))
	}
	var texts []string
	var walk func(any)
	walk = func(v any) {
		switch typed := v.(type) {
		case map[string]any:
			for _, key := range []string{"text", "summary", "description", "result", "content"} {
				if s, ok := typed[key].(string); ok && strings.TrimSpace(s) != "" {
					texts = append(texts, strings.TrimSpace(s))
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		case string:
			if strings.TrimSpace(typed) != "" {
				texts = append(texts, strings.TrimSpace(typed))
			}
		}
	}
	walk(value)
	if len(texts) == 0 {
		return ""
	}
	return texts[0]
}

func callMCPExtractor(ctx context.Context, refs []mediaRef, extractor config.MultimodalExtractorConfig) ([]string, error) {
	timeout := time.Duration(extractor.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client, err := startMCP(callCtx, extractor)
	if err != nil {
		return nil, err
	}
	defer client.close()
	tools, err := client.listTools(callCtx)
	if err != nil {
		return nil, err
	}
	tool := selectTool(tools, extractor.ToolName)
	if tool.Name == "" {
		return nil, fmt.Errorf("multimodal adapter extractor %q has no MCP tool available", extractor.Name)
	}
	out := make([]string, 0, len(refs))
	for i, ref := range refs {
		args := buildToolArgs(tool, ref, extractor.Prompt)
		text, errCall := client.callTool(callCtx, tool.Name, args)
		if errCall != nil {
			return nil, errCall
		}
		if strings.TrimSpace(text) != "" {
			out = append(out, fmt.Sprintf("Visual input %d (%s: %s):\n%s", i+1, ref.Kind, ref.URL, strings.TrimSpace(text)))
		}
	}
	return out, nil
}

type mcpClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	nextID int
}

type mcpTool struct {
	Name       string
	InputKeys  []string
	Required   map[string]struct{}
	SchemaType string
}

func startMCP(ctx context.Context, extractor config.MultimodalExtractorConfig) (*mcpClient, error) {
	if strings.TrimSpace(extractor.Command) == "" {
		return nil, fmt.Errorf("multimodal adapter extractor %q command is not configured", extractor.Name)
	}
	cmd := exec.CommandContext(ctx, extractor.Command, extractor.Args...)
	cmd.Env = os.Environ()
	for key, value := range extractor.Env {
		key = strings.TrimSpace(key)
		if key != "" {
			cmd.Env = append(cmd.Env, key+"="+os.ExpandEnv(value))
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	client := &mcpClient{cmd: cmd, stdin: stdin, reader: bufio.NewReader(stdout), nextID: 1}
	if _, err = client.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "clirelay", "version": "dev"},
	}); err != nil {
		client.close()
		if strings.TrimSpace(stderr.String()) != "" {
			return nil, fmt.Errorf("multimodal adapter initialize: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("multimodal adapter initialize: %w", err)
	}
	_ = client.notify(ctx, "notifications/initialized", map[string]any{})
	return client, nil
}

func (c *mcpClient) listTools(ctx context.Context) ([]mcpTool, error) {
	result, err := c.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	rawTools, _ := result["tools"].([]any)
	tools := make([]mcpTool, 0, len(rawTools))
	for _, raw := range rawTools {
		toolMap, _ := raw.(map[string]any)
		name, _ := toolMap["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		tool := mcpTool{Name: strings.TrimSpace(name), Required: map[string]struct{}{}}
		if schema, _ := toolMap["inputSchema"].(map[string]any); schema != nil {
			if typ, _ := schema["type"].(string); typ != "" {
				tool.SchemaType = typ
			}
			if props, _ := schema["properties"].(map[string]any); props != nil {
				for key := range props {
					tool.InputKeys = append(tool.InputKeys, key)
				}
				sort.Strings(tool.InputKeys)
			}
			if required, _ := schema["required"].([]any); required != nil {
				for _, item := range required {
					if key, _ := item.(string); key != "" {
						tool.Required[key] = struct{}{}
					}
				}
			}
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func selectTool(tools []mcpTool, configured string) mcpTool {
	configured = strings.TrimSpace(configured)
	for _, tool := range tools {
		if configured != "" && strings.EqualFold(tool.Name, configured) {
			return tool
		}
	}
	if configured != "" {
		return mcpTool{}
	}
	for _, tool := range tools {
		lower := strings.ToLower(tool.Name)
		if strings.Contains(lower, "vision") || strings.Contains(lower, "image") || strings.Contains(lower, "visual") || strings.Contains(lower, "ocr") {
			return tool
		}
	}
	if len(tools) > 0 {
		return tools[0]
	}
	return mcpTool{}
}

func buildToolArgs(tool mcpTool, ref mediaRef, prompt string) map[string]any {
	args := map[string]any{}
	keys := append([]string(nil), tool.InputKeys...)
	if len(keys) == 0 {
		keys = []string{"image_url", "file_url", "prompt"}
	}
	if key := chooseMediaArg(keys, ref.Kind, ref.URL); key != "" {
		args[key] = ref.URL
	}
	if key := choosePromptArg(keys); key != "" {
		args[key] = prompt
	}
	for key := range tool.Required {
		if _, ok := args[key]; !ok {
			args[key] = prompt
		}
	}
	return args
}

func chooseMediaArg(keys []string, kind, ref string) string {
	preferred := []string{}
	if kind == "image" {
		preferred = []string{"image_url", "image", "url", "path", "image_path", "file", "file_path"}
	} else if kind == "file" {
		preferred = []string{"file_url", "file", "url", "path", "file_path"}
	} else {
		preferred = []string{"video_url", "video", "url", "path", "video_path", "file", "file_path"}
	}
	if strings.HasPrefix(ref, "file://") || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, ".") {
		preferred = append([]string{"path", kind + "_path", "file_path"}, preferred...)
	}
	for _, want := range preferred {
		for _, key := range keys {
			if strings.EqualFold(key, want) {
				return key
			}
		}
	}
	for _, key := range keys {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "url") || strings.Contains(lower, "path") || strings.Contains(lower, kind) || strings.Contains(lower, "file") {
			return key
		}
	}
	return ""
}

func choosePromptArg(keys []string) string {
	for _, want := range []string{"prompt", "query", "instruction", "instructions", "question"} {
		for _, key := range keys {
			if strings.EqualFold(key, want) {
				return key
			}
		}
	}
	return ""
}

func (c *mcpClient) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	result, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	if isError, _ := result["isError"].(bool); isError {
		return "", fmt.Errorf("multimodal adapter MCP tool %s returned error: %s", name, extractMCPContentText(result))
	}
	return extractMCPContentText(result), nil
}

func extractMCPContentText(result map[string]any) string {
	content, _ := result["content"].([]any)
	var parts []string
	for _, item := range content {
		m, _ := item.(map[string]any)
		if text, _ := m["text"].(string); strings.TrimSpace(text) != "" {
			parts = append(parts, strings.TrimSpace(text))
		}
	}
	return strings.Join(parts, "\n")
}

func (c *mcpClient) request(ctx context.Context, method string, params any) (map[string]any, error) {
	id := c.nextID
	c.nextID++
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		msg, err := c.read(ctx)
		if err != nil {
			return nil, err
		}
		if intFromAny(msg["id"]) != id {
			continue
		}
		if rawErr, ok := msg["error"].(map[string]any); ok {
			return nil, fmt.Errorf("MCP %s error: %v", method, rawErr["message"])
		}
		result, _ := msg["result"].(map[string]any)
		return result, nil
	}
}

func (c *mcpClient) notify(ctx context.Context, method string, params any) error {
	_ = ctx
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *mcpClient) write(msg map[string]any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(data), data)
	return err
}

func (c *mcpClient) read(ctx context.Context) (map[string]any, error) {
	type result struct {
		msg map[string]any
		err error
	}
	ch := make(chan result, 1)
	go func() {
		msg, err := c.readOne()
		ch <- result{msg: msg, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case got := <-ch:
		return got.msg, got.err
	}
}

func (c *mcpClient) readOne() (map[string]any, error) {
	contentLength := -1
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(parts[0]), "content-length") {
			n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err == nil {
				contentLength = n
			}
		}
	}
	if contentLength < 0 {
		return nil, fmt.Errorf("MCP response missing Content-Length")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(c.reader, body); err != nil {
		return nil, err
	}
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func (c *mcpClient) close() {
	if c == nil {
		return
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
	}
}

func intFromAny(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		i, _ := typed.Int64()
		return int(i)
	default:
		return 0
	}
}

func limitStringBytes(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	const marker = "\n...[truncated visual context]...\n"
	edge := (maxBytes - len(marker)) / 2
	if edge <= 0 {
		return marker
	}
	return safePrefix(value, edge) + marker + safeSuffix(value, edge)
}

func safePrefix(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	end := 0
	for i := range value {
		if i > maxBytes {
			break
		}
		end = i
	}
	return value[:end]
}

func safeSuffix(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	start := len(value)
	for i := range value {
		if len(value)-i <= maxBytes {
			start = i
			break
		}
	}
	return value[start:]
}
