package main

import "os"

func composeFileHasService(path string, service string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return hasComposeService(string(data), service)
}
