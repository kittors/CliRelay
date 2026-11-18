package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// DoQwenLogin handles the Qwen device flow using the shared authentication manager.
func DoQwenLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}
	manager := newAuthManager()
	promptFn := options.Prompt
	if promptFn == nil {
		promptFn = defaultProjectPrompt()
	}
	authOpts := &sdkAuth.LoginOptions{
		NoBrowser:    options.NoBrowser,
		CallbackPort: options.CallbackPort,
		Metadata:     map[string]string{},
		Prompt:       promptFn,
	}
	_, savedPath, err := manager.Login(context.Background(), "qwen", cfg, authOpts)
	if err != nil {
		var emailErr *sdkAuth.EmailRequiredError
		if errors.As(err, &emailErr) {
			log.Error(emailErr.Error())
			return
		}
		fmt.Printf("Qwen authentication failed: %v\n", err)
		return
	}
	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	fmt.Println("Qwen authentication successful!")
}
