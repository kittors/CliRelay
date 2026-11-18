package authfiles

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const invalidFilePathPlaceholder = "__invalid_auth_file_name__"

const systemTenantID = "00000000-0000-0000-0000-000000000001"

func NormalizeTenantID(tenantID string) string {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return systemTenantID
	}
	return tenantID
}

func TenantAuthDir(authDir, tenantID string) string {
	if strings.TrimSpace(tenantID) == "" {
		return authDir
	}
	return filepath.Join(authDir, NormalizeTenantID(tenantID))
}

func TenantFilePath(authDir, tenantID, name string) string {
	return FilePath(TenantAuthDir(authDir, tenantID), name)
}

// Existing installations stored system credentials directly in authDir. Prefer
// the tenant directory, but keep the legacy root readable until startup migration
// has moved every file.
func ExistingTenantFilePath(authDir, tenantID, name string) string {
	path := TenantFilePath(authDir, tenantID, name)
	if _, err := os.Stat(path); err == nil || NormalizeTenantID(tenantID) != systemTenantID {
		return path
	}
	return FilePath(authDir, name)
}

func OpenTenantFile(authDir, tenantID, name string) (*os.File, os.FileInfo, error) {
	path := ExistingTenantFilePath(authDir, tenantID, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("auth file is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("auth file is not a regular file")
	}
	return file, openedInfo, nil
}

func ValidateFileQueryName(name string, requireJSON bool) (string, error) {
	name = strings.TrimSpace(name)
	if !isSafeSingleFileName(name) {
		return "", fmt.Errorf("invalid name")
	}
	if requireJSON && !IsJSONFileName(name) {
		return "", fmt.Errorf("name must end with .json")
	}
	return name, nil
}

func ValidateUploadedFileName(filename string) (string, error) {
	name := singleFileBaseName(filename)
	if !isSafeSingleFileName(name) {
		return "", fmt.Errorf("invalid name")
	}
	if !IsJSONFileName(name) {
		return "", fmt.Errorf("file must be .json")
	}
	return name, nil
}

func singleFileBaseName(name string) string {
	return path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
}

func isSafeSingleFileName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return false
	}
	if strings.ContainsAny(name, "\r\n\t") {
		return false
	}
	return filepath.Base(name) == name && singleFileBaseName(name) == name
}

func IsDeleteAllValue(value string) bool {
	return value == "true" || value == "1" || value == "*"
}

func IsJSONFileName(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".json")
}

func FilePath(authDir, name string) string {
	baseName := singleFileBaseName(name)
	if !isSafeSingleFileName(baseName) {
		baseName = invalidFilePathPlaceholder
	}
	full := filepath.Join(authDir, baseName)
	if filepath.IsAbs(full) {
		return full
	}
	if abs, errAbs := filepath.Abs(full); errAbs == nil {
		return abs
	}
	return full
}

func AuthIDForPath(authDir, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.TrimSpace(authDir) == "" {
		return filepath.Clean(path)
	}
	resolvedAuthDir, errResolve := util.ResolveAuthDir(authDir)
	if errResolve != nil || strings.TrimSpace(resolvedAuthDir) == "" {
		return filepath.Clean(path)
	}
	if !filepath.IsAbs(resolvedAuthDir) {
		if abs, errAbs := filepath.Abs(resolvedAuthDir); errAbs == nil {
			resolvedAuthDir = abs
		}
	}
	if evaluated, errEval := filepath.EvalSymlinks(resolvedAuthDir); errEval == nil {
		resolvedAuthDir = evaluated
	}
	normalizedPath := filepath.Clean(path)
	if !filepath.IsAbs(normalizedPath) {
		if abs, errAbs := filepath.Abs(normalizedPath); errAbs == nil {
			normalizedPath = abs
		}
	}
	if evaluated, errEval := filepath.EvalSymlinks(normalizedPath); errEval == nil {
		normalizedPath = evaluated
	}
	if rel, err := filepath.Rel(resolvedAuthDir, normalizedPath); err == nil && rel != "" && rel != "." && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return normalizedPath
}
