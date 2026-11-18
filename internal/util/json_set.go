package util

import "github.com/tidwall/sjson"

func setRawAt(jsonStr, path, value string) string {
	if path == "" {
		return value
	}
	result, _ := sjson.SetRaw(jsonStr, path, value)
	return result
}
