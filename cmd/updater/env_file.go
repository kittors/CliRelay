package main

import (
	"context"
	"fmt"
	"os"
	"strings"
)

func configuredImageInFile(envFile string) string {
	if strings.TrimSpace(envFile) == "" {
		return ""
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		return ""
	}
	return configuredImageRef(splitEnvLines(string(data)))
}

func restoreRequestedImage(ctx context.Context, envFile string, imageRef string, reporter updateReporter, updateErr error) error {
	if strings.TrimSpace(envFile) == "" || strings.TrimSpace(imageRef) == "" {
		return updateErr
	}
	if err := writeEnvKey(ctx, envFile, "CLI_PROXY_IMAGE", imageRef, reporter); err != nil {
		return fmt.Errorf("%w; restore previous CLI_PROXY_IMAGE failed: %v", updateErr, err)
	}
	reporter.Log("stdout", "restored previous CLI_PROXY_IMAGE after failed update")
	return updateErr
}

func writeEnvKey(ctx context.Context, envFile string, key string, value string, reporter updateReporter) error {
	data, err := os.ReadFile(envFile)
	if err != nil {
		return err
	}
	lines := splitEnvLines(string(data))
	line := key + "=" + value
	for i, existing := range lines {
		currentKey, _, ok := strings.Cut(existing, "=")
		if ok && strings.TrimSpace(currentKey) == key {
			lines[i] = line
			return writeDeploymentFile(ctx, envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600, reporter)
		}
	}
	lines = append(lines, line)
	return writeDeploymentFile(ctx, envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600, reporter)
}
