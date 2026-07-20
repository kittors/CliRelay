package management

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func (h *Handler) ListAuthFiles(c *gin.Context) {
	if h == nil {
		c.JSON(500, gin.H{"error": "handler not initialized"})
		return
	}
	if h.authManager == nil {
		h.listAuthFilesFromDisk(c)
		return
	}
	auths := h.authManager.ListForTenant(effectiveTenantID(c))
	// auth_subject_id is set once in BuildEntry (canonical server identity).
	files := managementauthfiles.ListEntries(auths, managementauthfiles.EntryOptions{
		OnStatError: func(path string, err error) {
			log.WithError(err).Warnf("failed to stat auth file %s", path)
		},
	})
	h.enrichAuthFileIdentityFingerprintSummaries(files, auths)
	c.JSON(200, gin.H{"files": files})
}

// GetAuthFileModels returns the models supported by a specific auth file.
//
// Query params:
//   - name (required): auth file name or auth ID
//   - refresh=1|true: force re-fetch live models from upstream.
//     xai/antigravity: updates runtime registry when successful.
//     claude/codex: provider-level discovery cache (shared by same-type accounts);
//     open auto-warms once and subsequent opens reuse cache; force refreshes
//     cache. Does NOT RegisterClient-replace the static channel catalog.
//     Falls back to registry on failure.
func (h *Handler) GetAuthFileModels(c *gin.Context) {
	name := c.Query("name")
	if name == "" {
		c.JSON(400, gin.H{"error": "name is required"})
		return
	}

	refresh := false
	switch strings.ToLower(strings.TrimSpace(c.Query("refresh"))) {
	case "1", "true", "yes", "force":
		refresh = true
	}

	reg := registry.GetGlobalRegistry()
	tenantID := effectiveTenantID(c)
	models, source := managementauthfiles.ListModelEntriesLiveForTenant(
		c.Request.Context(),
		h.authManager,
		reg,
		reg,
		h.cfg,
		tenantID,
		name,
		refresh,
	)
	c.JSON(200, gin.H{"models": models, "source": source})
}

// List auth files from disk when the auth manager is unavailable.
func (h *Handler) listAuthFilesFromDisk(c *gin.Context) {
	files, err := managementauthfiles.ListTenantDiskEntries(h.cfg.AuthDir, effectiveTenantID(c), time.Now())
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read auth dir: %v", err)})
		return
	}
	c.JSON(200, gin.H{"files": files})
}

// Download single auth file by name
func (h *Handler) DownloadAuthFile(c *gin.Context) {
	name, errValidate := managementauthfiles.ValidateFileQueryName(c.Query("name"), true)
	if errValidate != nil {
		c.JSON(400, gin.H{"error": errValidate.Error()})
		return
	}
	file, info, err := managementauthfiles.OpenTenantFile(h.cfg.AuthDir, effectiveTenantID(c), name)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(404, gin.H{"error": "file not found"})
		} else {
			c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read file: %v", err)})
		}
		return
	}
	defer file.Close()
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(c.Writer, c.Request, name, info.ModTime(), file)
}

// Upload auth file: multipart or raw JSON with ?name=
func (h *Handler) UploadAuthFile(c *gin.Context) {
	service := newAuthFileUploadService(h, c)
	if !service.Available() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	ctx := c.Request.Context()
	if c.Request != nil && c.Request.Body != nil && c.Writer != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, bodyutil.AuthFileBodyLimit+(64<<10))
	}
	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		file, err := c.FormFile("file")
		if err != nil {
			if bodyutil.IsTooLarge(err) {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file too large"})
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "file required"})
			return
		}
		if file.Size > bodyutil.AuthFileBodyLimit {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file too large"})
			return
		}
		if _, errValidate := service.ValidateMultipartFilename(file.Filename); errValidate != nil {
			writeAuthFileUploadError(c, errValidate)
			return
		}
		src, errOpen := file.Open()
		if errOpen != nil {
			c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read file: %v", errOpen)})
			return
		}
		defer func() {
			if errClose := src.Close(); errClose != nil {
				log.WithError(errClose).Warn("failed to close uploaded auth file")
			}
		}()
		data, errRead := bodyutil.ReadAll(src, bodyutil.AuthFileBodyLimit)
		if errRead != nil {
			if bodyutil.IsTooLarge(errRead) {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file too large"})
				return
			}
			c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read file: %v", errRead)})
			return
		}
		if _, errUpload := service.UploadMultipart(ctx, file.Filename, data); errUpload != nil {
			writeAuthFileUploadError(c, errUpload)
			return
		}
		c.JSON(200, gin.H{"status": "ok"})
		return
	}
	rawName, errValidate := service.ValidateRawName(c.Query("name"))
	if errValidate != nil {
		writeAuthFileUploadError(c, errValidate)
		return
	}
	data, err := bodyutil.ReadRequestBody(c, bodyutil.AuthFileBodyLimit)
	if err != nil {
		if bodyutil.IsTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
			return
		}
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	if _, errUpload := service.UploadRaw(ctx, rawName, data); errUpload != nil {
		writeAuthFileUploadError(c, errUpload)
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

// Delete auth files: single by name or all
func (h *Handler) DeleteAuthFile(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	ctx := c.Request.Context()
	service := managementauthfiles.DeleteService{
		AuthDir:    h.cfg.AuthDir,
		TenantID:   effectiveTenantID(c),
		Manager:    h.authManager,
		Repository: h.authFileRepository(),
		RemoveChannels: func(channels []string) error {
			return h.removeChannelReferencesForTenant(effectiveTenantID(c), channels)
		},
	}
	if managementauthfiles.IsDeleteAllValue(c.Query("all")) {
		result, err := service.DeleteAll(ctx)
		if err != nil && result.Deleted == 0 {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		payload := gin.H{"status": "ok", "deleted": result.Deleted}
		if err != nil {
			log.WithError(err).Warn("auth files deleted with cleanup warning")
			payload["warning"] = err.Error()
		}
		c.JSON(200, payload)
		return
	}
	name, errValidate := managementauthfiles.ValidateFileQueryName(c.Query("name"), false)
	if errValidate != nil {
		c.JSON(400, gin.H{"error": errValidate.Error()})
		return
	}
	result, errDelete := service.DeleteOne(ctx, name)
	if errDelete != nil && result.Deleted == 0 {
		if errors.Is(errDelete, managementauthfiles.ErrAuthFileNotFound) {
			c.JSON(404, gin.H{"error": "file not found"})
		} else {
			c.JSON(500, gin.H{"error": errDelete.Error()})
		}
		return
	}
	payload := gin.H{"status": "ok", "deleted": result.Deleted}
	if errDelete != nil {
		log.WithError(errDelete).Warnf("auth file %s deleted with cleanup warning", name)
		payload["warning"] = errDelete.Error()
	}
	c.JSON(200, payload)
}

func newAuthFileUploadService(h *Handler, c *gin.Context) managementauthfiles.UploadService {
	authDir := ""
	repository := managementauthfiles.Repository{}
	if h != nil && h.cfg != nil {
		authDir = h.cfg.AuthDir
	}
	var manager *coreauth.Manager
	if h != nil {
		manager = h.authManager
		repository = h.authFileRepository()
	}
	return managementauthfiles.UploadService{
		AuthDir:    authDir,
		TenantID:   effectiveTenantID(c),
		Manager:    manager,
		Repository: repository,
	}
}

func writeAuthFileUploadError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, managementauthfiles.ErrAuthManagerUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
	case managementauthfiles.IsUploadValidationError(err):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// PatchAuthFileStatus toggles the disabled state of an auth file
func (h *Handler) PatchAuthFileStatus(c *gin.Context) {
	service := newAuthFilePatchService(h, c)
	if !service.Available() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name     string `json:"name"`
		Disabled *bool  `json:"disabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	result, errPatch := service.PatchStatus(c.Request.Context(), managementauthfiles.StatusPatch{
		Name:     req.Name,
		Disabled: req.Disabled,
	})
	if errPatch != nil {
		writeAuthFilePatchError(c, errPatch)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "disabled": result.Disabled})
}

// PatchAuthFileFields updates editable fields of an auth file.
func (h *Handler) PatchAuthFileFields(c *gin.Context) {
	service := newAuthFilePatchService(h, c)
	if !service.Available() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req managementauthfiles.FieldPatch
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if errPatch := service.PatchFields(c.Request.Context(), req); errPatch != nil {
		writeAuthFilePatchError(c, errPatch)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func newAuthFilePatchService(h *Handler, c *gin.Context) managementauthfiles.PatchService {
	var manager *coreauth.Manager
	repository := managementauthfiles.Repository{}
	var validateLabel func(label, excludeAuthID string) (string, error)
	var renameChannels func(oldNames []string, newName string) error
	if h != nil {
		tenantID := effectiveTenantID(c)
		manager = h.authManager
		repository = h.authFileRepository()
		validateLabel = func(label, excludeAuthID string) (string, error) {
			return h.validateAuthChannelNameForTenant(tenantID, label, excludeAuthID)
		}
		renameChannels = func(oldNames []string, newName string) error {
			return h.renameChannelReferencesForTenant(tenantID, oldNames, newName)
		}
	}
	return managementauthfiles.PatchService{
		Manager:        manager,
		TenantID:       effectiveTenantID(c),
		Repository:     repository,
		ValidateLabel:  validateLabel,
		RenameChannels: renameChannels,
	}
}

func writeAuthFilePatchError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, managementauthfiles.ErrAuthManagerUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
	case errors.Is(err, managementauthfiles.ErrNameRequired):
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
	case errors.Is(err, managementauthfiles.ErrDisabledRequired):
		c.JSON(http.StatusBadRequest, gin.H{"error": "disabled is required"})
	case errors.Is(err, managementauthfiles.ErrAuthFileNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
	case managementauthfiles.IsInternalPatchError(err):
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}

func (h *Handler) authFileRepository() managementauthfiles.Repository {
	if h == nil {
		return managementauthfiles.Repository{}
	}
	store := h.tokenStore
	if store == nil {
		store = sdkAuth.GetTokenStore()
		h.tokenStore = store
	}
	baseDir := ""
	if h.cfg != nil {
		baseDir = h.cfg.AuthDir
	}
	return managementauthfiles.Repository{
		Store:        store,
		BaseDir:      baseDir,
		PostAuthHook: h.postAuthHook,
	}
}
