package authfiles

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

var ErrAuthFileNotFound = errors.New("auth file not found")

type DeleteService struct {
	AuthDir        string
	TenantID       string
	Manager        *coreauth.Manager
	Repository     Repository
	RemoveChannels func([]string) error
}

type DeleteResult struct {
	Deleted int
}

func (s DeleteService) DeleteAll(ctx context.Context) (DeleteResult, error) {
	names := make(map[string]struct{})
	if s.Manager != nil {
		for _, auth := range s.Manager.ListForTenant(NormalizeTenantID(s.TenantID)) {
			if auth == nil || IsRuntimeOnly(auth) {
				continue
			}
			if name := PublicFileName(auth); name != "" {
				names[name] = struct{}{}
			}
		}
	}

	authDir := TenantAuthDir(s.AuthDir, s.TenantID)
	entries, err := os.ReadDir(authDir)
	if err != nil && !os.IsNotExist(err) {
		return DeleteResult{}, fmt.Errorf("failed to read auth dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && IsJSONFileName(entry.Name()) {
			names[entry.Name()] = struct{}{}
		}
	}

	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	result := DeleteResult{}
	var cleanupErrors []error
	for _, name := range ordered {
		one, errDelete := s.DeleteOne(ctx, name)
		result.Deleted += one.Deleted
		if errDelete == nil {
			continue
		}
		if one.Deleted > 0 {
			cleanupErrors = append(cleanupErrors, errDelete)
			continue
		}
		return result, errDelete
	}
	return result, errors.Join(cleanupErrors...)
}

func (s DeleteService) DeleteOne(ctx context.Context, name string) (DeleteResult, error) {
	name, errValidate := ValidateFileQueryName(name, false)
	if errValidate != nil {
		return DeleteResult{}, errValidate
	}

	target := FindByNameOrIDForTenant(s.Manager, s.TenantID, name)
	full := ExistingTenantFilePath(s.AuthDir, s.TenantID, name)
	if target != nil {
		if path := Attribute(target, "path"); path != "" {
			full = path
		}
	} else if _, errStat := os.Lstat(full); errStat != nil {
		if os.IsNotExist(errStat) {
			return DeleteResult{}, ErrAuthFileNotFound
		}
		return DeleteResult{}, fmt.Errorf("failed to inspect auth file: %w", errStat)
	}

	// The store owns durable deletion (Git/PostgreSQL/object storage included).
	// Removing the mirror first prevents stores such as GitTokenStore from seeing
	// the deletion and committing it, so local cleanup intentionally happens last.
	if errDelete := s.Repository.Delete(ctx, full); errDelete != nil {
		return DeleteResult{}, errDelete
	}
	if errRemove := os.Remove(full); errRemove != nil && !os.IsNotExist(errRemove) {
		return DeleteResult{}, fmt.Errorf("failed to remove local auth file: %w", errRemove)
	}

	deletedChannels := DeletedChannelIdentifiers(target)
	if target != nil && s.Manager != nil {
		_, _ = s.Manager.Delete(coreauth.WithSkipPersist(ctx), target.ID)
	}
	result := DeleteResult{Deleted: 1}
	if errCleanup := s.removeChannelReferences(deletedChannels); errCleanup != nil {
		// The credential is already durably deleted. Preserve that outcome so the
		// caller can remove stale UI state while surfacing the cleanup warning.
		return result, fmt.Errorf("auth file deleted but channel reference cleanup failed: %w", errCleanup)
	}
	return result, nil
}

func (s DeleteService) removeChannelReferences(channels []string) error {
	if len(channels) == 0 || s.RemoveChannels == nil {
		return nil
	}
	return s.RemoveChannels(channels)
}
