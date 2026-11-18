package iflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	internaliflow "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/iflow"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

var (
	ErrCookieRequired = errors.New("cookie is required")
	ErrDuplicateCheck = errors.New("failed to check duplicate")
	ErrExtractEmail   = errors.New("failed to extract email from token")
	ErrSaveTokens     = errors.New("failed to save authentication tokens")
)

type DuplicateBXAuthError struct {
	ExistingFile string
}

func (e DuplicateBXAuthError) Error() string { return "duplicate BXAuth found" }

func (e DuplicateBXAuthError) ExistingFileName() string {
	return filepath.Base(e.ExistingFile)
}

type CookieAuth interface {
	AuthenticateWithCookie(context.Context, string) (*internaliflow.IFlowTokenData, error)
	CreateCookieTokenStorage(*internaliflow.IFlowTokenData) *internaliflow.IFlowTokenStorage
}

type DuplicateBXAuthFunc func(authDir, bxAuth string) (string, error)

type CookieLoginOptions struct {
	Config         *config.Config
	Auth           CookieAuth
	AuthDir        string
	CheckDuplicate DuplicateBXAuthFunc
	SaveRecord     SaveRecordFunc
	Now            func() time.Time
}

type CookieLoginResult struct {
	SavedPath string
	Email     string
	Expired   string
	Type      string
}

func AuthenticateCookie(ctx context.Context, rawCookie string, opts CookieLoginOptions) (CookieLoginResult, error) {
	cookieValue := strings.TrimSpace(rawCookie)
	if cookieValue == "" {
		return CookieLoginResult{}, ErrCookieRequired
	}

	cookieValue, errNormalize := internaliflow.NormalizeCookie(cookieValue)
	if errNormalize != nil {
		return CookieLoginResult{}, errNormalize
	}

	authDir := opts.AuthDir
	if authDir == "" && opts.Config != nil {
		authDir = opts.Config.AuthDir
	}
	checkDuplicate := opts.CheckDuplicate
	if checkDuplicate == nil {
		checkDuplicate = internaliflow.CheckDuplicateBXAuth
	}
	bxAuth := internaliflow.ExtractBXAuth(cookieValue)
	if existingFile, err := checkDuplicate(authDir, bxAuth); err != nil {
		return CookieLoginResult{}, fmt.Errorf("%w: %v", ErrDuplicateCheck, err)
	} else if existingFile != "" {
		return CookieLoginResult{}, DuplicateBXAuthError{ExistingFile: existingFile}
	}

	auth := opts.Auth
	if auth == nil {
		auth = internaliflow.NewIFlowAuth(opts.Config)
	}
	tokenData, errAuth := auth.AuthenticateWithCookie(ctx, cookieValue)
	if errAuth != nil {
		return CookieLoginResult{}, errAuth
	}
	tokenData.Cookie = cookieValue

	tokenStorage := auth.CreateCookieTokenStorage(tokenData)
	if tokenStorage == nil {
		return CookieLoginResult{}, ErrExtractEmail
	}
	email := strings.TrimSpace(tokenStorage.Email)
	if email == "" {
		return CookieLoginResult{}, ErrExtractEmail
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	record := CookieRecordFromTokenStorage(tokenStorage, now())
	if opts.SaveRecord == nil {
		return CookieLoginResult{}, ErrSaveTokens
	}
	savedPath, errSave := opts.SaveRecord(ctx, record)
	if errSave != nil {
		return CookieLoginResult{}, fmt.Errorf("%w: %v", ErrSaveTokens, errSave)
	}

	fmt.Printf("iFlow cookie authentication successful. Token saved to %s\n", savedPath)
	return CookieLoginResult{
		SavedPath: savedPath,
		Email:     email,
		Expired:   tokenStorage.Expire,
		Type:      tokenStorage.Type,
	}, nil
}
