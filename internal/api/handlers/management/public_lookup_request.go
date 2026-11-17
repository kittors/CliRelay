package management

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
)

const publicLookupBodyLimit int64 = 8 << 10

type publicLookupRequest struct {
	APIKey         string   `json:"api_key"`
	Days           int      `json:"days"`
	Page           int      `json:"page"`
	Size           int      `json:"size"`
	Model          string   `json:"model"`
	Models         []string `json:"models"`
	APIKeyIDs      []string `json:"api_key_ids"`
	Channel        string   `json:"channel"`
	ChannelName    string   `json:"channel_name"`
	Channels       []string `json:"channels"`
	Status         string   `json:"status"`
	Statuses       []string `json:"statuses"`
	APIKeyIDsEmpty bool     `json:"api_key_ids_empty"`
	ModelsEmpty    bool     `json:"models_empty"`
	ChannelsEmpty  bool     `json:"channels_empty"`
	StatusesEmpty  bool     `json:"statuses_empty"`
	// ClientNames selects the caller's requests by client, like the management
	// view's client_names.
	ClientNames      []string `json:"client_names"`
	ClientNamesEmpty bool     `json:"client_names_empty"`
	Part             string   `json:"part"`
	Format           string   `json:"format"`
}

func readPublicLookupRequest(c *gin.Context) (publicLookupRequest, int, string) {
	req := publicLookupRequest{}
	if c == nil || c.Request == nil {
		return req, http.StatusInternalServerError, "request unavailable"
	}

	if c.Request.Method == http.MethodPost {
		body, err := bodyutil.ReadRequestBody(c, publicLookupBodyLimit)
		if err != nil {
			if bodyutil.IsTooLarge(err) {
				return req, http.StatusRequestEntityTooLarge, "request body too large"
			}
			return req, http.StatusBadRequest, "failed to read request body"
		}
		if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
			if err := json.Unmarshal(body, &req); err != nil {
				return req, http.StatusBadRequest, "invalid json body"
			}
		}
	}

	req.APIKey = strings.TrimSpace(req.APIKey)

	if req.Page < 1 {
		req.Page = intQueryDefault(c, "page", 1)
	}
	if req.Size < 1 {
		req.Size = intQueryDefault(c, "size", 50)
	}
	if req.Days < 1 {
		req.Days = intQueryDefault(c, "days", 7)
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = strings.TrimSpace(c.Query("model"))
	}
	req.Models = append(req.Models, queryStringListMulti(c, "model", "models")...)
	req.APIKeyIDs = append(req.APIKeyIDs, queryStringListMulti(c, "api_key_id", "api_key_ids")...)
	if strings.TrimSpace(req.Status) == "" {
		req.Status = strings.TrimSpace(c.Query("status"))
	}
	req.Statuses = append(req.Statuses, queryStringListMulti(c, "status", "statuses")...)
	if strings.TrimSpace(req.Channel) == "" {
		req.Channel = strings.TrimSpace(c.Query("channel"))
	}
	if strings.TrimSpace(req.ChannelName) == "" {
		req.ChannelName = strings.TrimSpace(c.Query("channel_name"))
	}
	req.Channels = append(req.Channels, queryStringListMulti(c, "channel", "channels")...)
	if req.ChannelName != "" {
		req.Channels = append(req.Channels, req.ChannelName)
	}
	if strings.TrimSpace(req.Part) == "" {
		req.Part = strings.TrimSpace(c.Query("part"))
	}
	if strings.TrimSpace(req.Format) == "" {
		req.Format = strings.TrimSpace(c.Query("format"))
	}

	req.Model = strings.TrimSpace(req.Model)
	if req.Model != "" {
		req.Models = append(req.Models, req.Model)
	}
	req.Models = dedupePublicLookupStrings(req.Models)
	req.APIKeyIDs = dedupePublicLookupStrings(req.APIKeyIDs)
	req.Status = strings.TrimSpace(req.Status)
	if req.Status != "" {
		req.Statuses = append(req.Statuses, req.Status)
	}
	req.Statuses = dedupePublicLookupStrings(req.Statuses)
	req.Channel = strings.TrimSpace(req.Channel)
	if req.Channel != "" {
		req.Channels = append(req.Channels, req.Channel)
	}
	req.Channels = dedupePublicLookupStrings(req.Channels)
	req.ClientNames = dedupePublicLookupStrings(append(req.ClientNames, queryStringListMulti(c, "client_name", "client_names")...))
	req.ClientNamesEmpty = req.ClientNamesEmpty || queryBool(c, "client_names_empty")
	req.APIKeyIDsEmpty = req.APIKeyIDsEmpty || queryBool(c, "api_key_ids_empty")
	req.ModelsEmpty = req.ModelsEmpty || queryBool(c, "models_empty")
	req.ChannelsEmpty = req.ChannelsEmpty || queryBool(c, "channels_empty")
	req.StatusesEmpty = req.StatusesEmpty || queryBool(c, "statuses_empty")
	req.Part = normalizeLogContentPartValue(req.Part)
	req.Format = normalizeLogContentFormatValue(req.Format)

	return req, 0, ""
}

func dedupePublicLookupStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, raw := range values {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}
