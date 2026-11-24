package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/vertex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// ImportVertexCredential handles uploading a Vertex service account JSON and saving it as an auth record.
func (h *Handler) ImportVertexCredential(c *gin.Context) {
	if h == nil || h.cfg == nil {
		writeServiceUnavailableError(c, "配置服务")
		return
	}
	if h.cfg.AuthDir == "" {
		writeServiceUnavailableError(c, "凭据存储目录（auth-dir）")
		return
	}

	if c.Request != nil && c.Request.Body != nil && c.Writer != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, bodyutil.VertexCredentialBodyLimit+(64<<10))
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		if bodyutil.IsTooLarge(err) {
			writeReadBodyError(c, err, bodyutil.VertexCredentialBodyLimit)
			return
		}
		writeMissingFieldError(c, "file", "请通过表单字段 file 上传 Google Cloud Vertex Service Account JSON 文件")
		return
	}
	if fileHeader.Size > bodyutil.VertexCredentialBodyLimit {
		writeError(c, http.StatusRequestEntityTooLarge, "file_too_large",
			fmt.Sprintf("上传的 Vertex 凭据文件过大，超出 %s 上限，请确认选择了正确的 Service Account JSON 文件", formatByteLimit(bodyutil.VertexCredentialBodyLimit)),
			map[string]any{"max_bytes": bodyutil.VertexCredentialBodyLimit},
		)
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		writeError(c, http.StatusBadRequest, "read_file_failed", "无法打开上传的 Vertex 凭据文件，请重新选择文件上传")
		return
	}
	defer file.Close()

	data, err := bodyutil.ReadAll(file, bodyutil.VertexCredentialBodyLimit)
	if err != nil {
		if bodyutil.IsTooLarge(err) {
			writeReadBodyError(c, err, bodyutil.VertexCredentialBodyLimit)
			return
		}
		writeError(c, http.StatusBadRequest, "read_file_failed", "读取上传的 Vertex 凭据文件内容失败，请重试")
		return
	}

	var serviceAccount map[string]any
	if err := json.Unmarshal(data, &serviceAccount); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json", "message": err.Error()})
		return
	}

	normalizedSA, err := vertex.NormalizeServiceAccountMap(serviceAccount)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid service account", "message": err.Error()})
		return
	}
	serviceAccount = normalizedSA

	projectID := strings.TrimSpace(valueAsString(serviceAccount["project_id"]))
	if projectID == "" {
		writeMissingFieldError(c, "project_id", "Vertex Service Account JSON 中缺少必填字段 project_id")
		return
	}
	email := strings.TrimSpace(valueAsString(serviceAccount["client_email"]))

	location := strings.TrimSpace(c.PostForm("location"))
	if location == "" {
		location = strings.TrimSpace(c.Query("location"))
	}
	if location == "" {
		location = "us-central1"
	}

	fileName := fmt.Sprintf("vertex-%s.json", sanitizeVertexFilePart(projectID))
	label := labelForVertex(projectID, email)
	storage := &vertex.VertexCredentialStorage{
		ServiceAccount: serviceAccount,
		ProjectID:      projectID,
		Email:          email,
		Location:       location,
		Type:           "vertex",
	}
	metadata := map[string]any{
		"service_account": serviceAccount,
		"project_id":      projectID,
		"email":           email,
		"location":        location,
		"type":            "vertex",
		"label":           label,
	}
	record := &coreauth.Auth{
		ID:       fileName,
		Provider: "vertex",
		FileName: fileName,
		Storage:  storage,
		Label:    label,
		Metadata: metadata,
	}

	ctx := requestAuthContext(c)
	savedPath, err := h.saveTokenRecord(ctx, record)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save_failed", "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"auth-file":  savedPath,
		"project_id": projectID,
		"email":      email,
		"location":   location,
	})
}

func valueAsString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func sanitizeVertexFilePart(s string) string {
	out := strings.TrimSpace(s)
	replacers := []string{"/", "_", "\\", "_", ":", "_", " ", "-"}
	for i := 0; i < len(replacers); i += 2 {
		out = strings.ReplaceAll(out, replacers[i], replacers[i+1])
	}
	if out == "" {
		return "vertex"
	}
	return out
}

func labelForVertex(projectID, email string) string {
	p := strings.TrimSpace(projectID)
	e := strings.TrimSpace(email)
	if p != "" && e != "" {
		return fmt.Sprintf("%s (%s)", p, e)
	}
	if p != "" {
		return p
	}
	if e != "" {
		return e
	}
	return "vertex"
}
