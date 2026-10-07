package translator

import (
	"bytes"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// minLeakedImageBytes is the shortest base64 run treated as a leaked image.
// Real screenshots are hundreds of kilobytes; 32 KiB keeps small inline icons
// in legitimate text out of the report.
const minLeakedImageBytes = 32 * 1024

var dataImageMarker = []byte("data:image/")

// warnOnLeakedImages logs when a translated request carries a base64 image
// inside a JSON string instead of an image block.
//
// Every translator converts image parts into its target's image type. A
// data:image URL surviving as string text means a translator flattened an
// image-bearing value with Result.String(), and the upstream will count the
// base64 as text: one screenshot becomes millions of tokens and the request
// fails as "prompt is too long", which points nowhere near the cause.
func warnOnLeakedImages(from, to Format, model string, out []byte) {
	if len(out) < minLeakedImageBytes || !bytes.Contains(out, dataImageMarker) {
		return
	}
	path, size := findLeakedImage(gjson.ParseBytes(out), "")
	if path == "" {
		return
	}
	log.Warnf("translator %s->%s (model %s): base64 image of %d bytes embedded as text at %s; the upstream will count it as text tokens", from, to, model, size, path)
}

// findLeakedImage returns the path of the first string value that embeds a
// large data:image URL anywhere but at its start, plus the value's length.
// A value that is exactly a data URL is a legitimate image field
// (image_url.url, source.url); one that contains it mid-string is JSON text.
func findLeakedImage(value gjson.Result, path string) (string, int) {
	switch {
	case value.Type == gjson.String:
		s := value.Str
		if len(s) >= minLeakedImageBytes {
			if idx := bytes.Index([]byte(s), dataImageMarker); idx > 0 {
				return path, len(s)
			}
		}
	case value.IsArray() || value.IsObject():
		var foundPath string
		var foundSize int
		value.ForEach(func(key, child gjson.Result) bool {
			childPath := key.String()
			if path != "" {
				childPath = path + "." + childPath
			}
			foundPath, foundSize = findLeakedImage(child, childPath)
			return foundPath == ""
		})
		return foundPath, foundSize
	}
	return "", 0
}
