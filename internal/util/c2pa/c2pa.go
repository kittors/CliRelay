// Package c2pa extracts a readable summary of the C2PA content credentials that
// generated images carry.
//
// Why a panel needs this: the Codex image endpoint ignores the model field of
// the request. Sending gpt-image-2.5-flare, gpt-image-2, or a model id that does
// not exist at all each return 200 with a picture, so a green "test passed" in
// the model catalog says nothing about which model actually served it. The C2PA
// manifest the upstream signs into the file does say — it carries the generator
// name and version the producer claims — which makes it the only evidence an
// operator has for telling 2.5 apart from 2.0.
//
// This is a summary extractor, not a validator. It does not verify signatures or
// trust chains, so its output must be presented as "what the file claims about
// itself", never as proof of provenance.
package c2pa

import (
	"encoding/binary"
	"strings"
	"unicode/utf8"
)

// Summary is what a manifest says about itself, flattened for display.
type Summary struct {
	// Present is false when the asset carries no manifest at all, which is itself
	// worth showing: it means the upstream returned an unsigned image.
	Present bool `json:"present"`
	// Generator and GeneratorVersion come from claim_generator_info. For OpenAI
	// images these read "gpt-image" / "2.0" even when 2.5 was requested.
	Generator        string `json:"generator,omitempty"`
	GeneratorVersion string `json:"generator_version,omitempty"`
	// Issuer is the signing organization named in the certificate chain.
	Issuer string `json:"issuer,omitempty"`
	// DigitalSourceType is the IPTC term the producer asserts, e.g.
	// trainedAlgorithmicMedia for a fully generated image.
	DigitalSourceType string `json:"digital_source_type,omitempty"`
	// Actions are the c2pa.actions entries, e.g. c2pa.created.
	Actions []string `json:"actions,omitempty"`
	// Fields carries every key/value pair recovered from the manifest, in the
	// order found. Extraction is best-effort, so this is kept as the operator's
	// escape hatch when the structured fields above come back empty.
	Fields []Field `json:"fields,omitempty"`
}

// Field is one recovered key/value pair from a manifest.
type Field struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Interesting manifest keys, in the order they are reported.
const (
	keyClaimGenerator = "claim_generator"
	keyGeneratorInfo  = "claim_generator_info"
	keyName           = "name"
	keyVersion        = "version"
	keySoftwareAgent  = "softwareAgent"
	keyAction         = "action"
	keyDigitalSource  = "digitalSourceType"
)

// maxScanBytes bounds the work done on one asset. A manifest sits in the first
// blocks of the file; scanning a multi-megabyte image past that only burns CPU
// on pixel data that cannot contain CBOR.
const maxScanBytes = 512 * 1024

// Extract reads whatever provenance an asset declares. It never fails: an asset
// with no manifest, a truncated manifest, or a container this build does not
// know all return a Summary with Present false rather than an error, because the
// probe's job is to report what it found, not to reject the image.
func Extract(asset []byte) Summary {
	block := manifestBlock(asset)
	if len(block) == 0 {
		return Summary{}
	}
	fields := scanCBORStrings(block)
	if len(fields) == 0 {
		return Summary{}
	}

	summary := Summary{Present: true, Fields: fields}
	// claim_generator_info and softwareAgent both hold an object rather than a
	// string, so the string that follows them is the nested "name" key, not the
	// generator. Those keys only open the scope; the value is taken from the
	// name/version pair inside it.
	generatorScope := false
	for _, field := range fields {
		switch field.Key {
		case keyGeneratorInfo, keyClaimGenerator, keySoftwareAgent:
			generatorScope = true
			if summary.Generator == "" && !isStructuralKey(field.Value) {
				summary.Generator = field.Value
			}
		case keyName:
			if generatorScope && summary.Generator == "" && !isStructuralKey(field.Value) {
				summary.Generator = field.Value
			}
		case keyVersion:
			if generatorScope && summary.GeneratorVersion == "" && !isStructuralKey(field.Value) {
				summary.GeneratorVersion = field.Value
			}
		case keyDigitalSource:
			if summary.DigitalSourceType == "" {
				summary.DigitalSourceType = field.Value
			}
		case keyAction:
			summary.Actions = appendUnique(summary.Actions, field.Value)
		}
	}
	summary.Issuer = issuerFromChain(block)
	summary.Actions = appendUnique(summary.Actions, claimActions(fields)...)
	return summary
}

// claimActions picks up c2pa.* action identifiers that appear as bare strings
// rather than as the value of an "action" key, which is how some producers
// serialize the actions assertion.
func claimActions(fields []Field) []string {
	actions := make([]string, 0, 2)
	for _, field := range fields {
		for _, candidate := range []string{field.Key, field.Value} {
			if strings.HasPrefix(candidate, "c2pa.") &&
				(strings.Contains(candidate, ".created") || strings.Contains(candidate, ".converted") ||
					strings.Contains(candidate, ".edited") || strings.Contains(candidate, ".placed")) {
				actions = append(actions, candidate)
			}
		}
	}
	return actions
}

func appendUnique(list []string, values ...string) []string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		duplicate := false
		for _, existing := range list {
			if existing == value {
				duplicate = true
				break
			}
		}
		if !duplicate {
			list = append(list, value)
		}
	}
	return list
}

// issuerFromChain reads the signing organization out of the embedded
// certificate. The certificate is DER inside the COSE signature, and parsing the
// whole chain to print one name would be disproportionate, so the organization
// string is recovered from its length prefix instead.
//
// Both DER strings and CBOR text strings put a length immediately before the
// bytes, so the byte in front of the match gives the exact end of the name.
// Without it the read runs into whatever token follows and reports
// "OpenAI Media Service APIdicon".
func issuerFromChain(block []byte) string {
	index := indexOf(block, []byte("OpenAI"))
	if index <= 0 {
		return ""
	}
	if length := int(block[index-1] & 0x7F); length >= len("OpenAI") && index+length <= len(block) {
		if candidate := string(block[index : index+length]); isPrintable(candidate) {
			return strings.TrimSpace(candidate)
		}
	}
	end := index
	for end < len(block) && end-index < 64 && isPrintableASCII(block[end]) {
		end++
	}
	return strings.TrimSpace(string(block[index:end]))
}

func indexOf(haystack, needle []byte) int {
	return strings.Index(string(haystack), string(needle))
}

// manifestBlock locates the JUMBF manifest store inside a container. PNG carries
// it in a caBX chunk and JPEG in APP11 segments; for anything else the raw bytes
// are scanned for the JUMBF magic so a format this build has not met still
// reports what it can.
func manifestBlock(asset []byte) []byte {
	if len(asset) == 0 {
		return nil
	}
	if block := pngManifest(asset); len(block) > 0 {
		return block
	}
	scan := asset
	if len(scan) > maxScanBytes {
		scan = scan[:maxScanBytes]
	}
	// "jumb" is the JUMBF superbox type; "c2pa" is the manifest store label that
	// follows it. Requiring both avoids matching the four letters in pixel noise.
	start := indexOf(scan, []byte("jumb"))
	if start < 0 || indexOf(scan, []byte("c2pa")) < 0 {
		return nil
	}
	return scan[start:]
}

var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// pngManifest walks the PNG chunk list to the caBX chunk that holds the C2PA
// manifest store. Walking chunks rather than searching bytes keeps a manifest
// that happens to sit after a large chunk reachable, and keeps pixel data that
// happens to spell "caBX" out of the result.
func pngManifest(asset []byte) []byte {
	if len(asset) < len(pngSignature) || string(asset[:len(pngSignature)]) != string(pngSignature) {
		return nil
	}
	offset := len(pngSignature)
	for offset+8 <= len(asset) {
		length := int(binary.BigEndian.Uint32(asset[offset : offset+4]))
		chunkType := string(asset[offset+4 : offset+8])
		dataStart := offset + 8
		if length < 0 || dataStart+length > len(asset) {
			return nil
		}
		if chunkType == "caBX" {
			return asset[dataStart : dataStart+length]
		}
		if chunkType == "IDAT" {
			// Pixel data has begun; C2PA is written ahead of it, so there is
			// nothing left to find and no reason to walk megabytes of scanlines.
			return nil
		}
		offset = dataStart + length + 4 // +4 for the chunk CRC
	}
	return nil
}

// scanCBORStrings recovers the manifest's text strings in order and pairs each
// recognized key with the string that follows it.
//
// A full CBOR decoder is deliberately not used. The manifest store nests CBOR
// inside JUMBF boxes inside a COSE envelope, and decoding all three layers to
// display a generator name would add a dependency and a large amount of code for
// a read-only diagnostic. Definite-length text strings are self-delimiting
// enough to recover reliably, and every value this reports is labeled in the UI
// as the file's own claim rather than a verified fact.
func scanCBORStrings(block []byte) []Field {
	strs := make([]string, 0, 64)
	for i := 0; i < len(block); {
		text, size := readCBORText(block[i:])
		if size == 0 {
			i++
			continue
		}
		strs = append(strs, text)
		i += size
	}

	fields := make([]Field, 0, len(strs))
	for i, text := range strs {
		if !isInterestingKey(text) {
			continue
		}
		value := ""
		if i+1 < len(strs) {
			value = strs[i+1]
		}
		fields = append(fields, Field{Key: text, Value: value})
	}
	return fields
}

func isInterestingKey(text string) bool {
	switch text {
	case keyClaimGenerator, keyGeneratorInfo, keyName, keyVersion,
		keySoftwareAgent, keyAction, keyDigitalSource:
		return true
	}
	return strings.HasPrefix(text, "c2pa.")
}

// isStructuralKey reports whether a recovered string is itself a manifest key.
// When it is, it is the opening key of a nested object rather than the value of
// the key before it, so reading it as a value would report "name" as the
// generator.
func isStructuralKey(text string) bool {
	switch text {
	case keyClaimGenerator, keyGeneratorInfo, keyName, keyVersion,
		keySoftwareAgent, keyAction, keyDigitalSource:
		return true
	}
	return text == ""
}

// readCBORText decodes one definite-length CBOR text string at the head of buf,
// returning the string and how many bytes it occupied. A zero size means buf does
// not start with a text string this reader accepts.
func readCBORText(buf []byte) (string, int) {
	if len(buf) == 0 {
		return "", 0
	}
	const majorText = 0x60
	head := buf[0]
	if head&0xE0 != majorText {
		return "", 0
	}
	info := int(head & 0x1F)
	offset := 1
	length := 0
	switch {
	case info < 24:
		length = info
	case info == 24:
		if len(buf) < 2 {
			return "", 0
		}
		length = int(buf[1])
		offset = 2
	case info == 25:
		if len(buf) < 3 {
			return "", 0
		}
		length = int(binary.BigEndian.Uint16(buf[1:3]))
		offset = 3
	default:
		// Longer or indefinite-length strings are not manifest keys or short
		// values, so they are skipped rather than decoded.
		return "", 0
	}
	// Single characters are almost always coincidence in binary data, and a
	// manifest key or generator name is never one byte.
	if length < 2 || offset+length > len(buf) {
		return "", 0
	}
	text := string(buf[offset : offset+length])
	if !utf8.ValidString(text) || !isPrintable(text) {
		return "", 0
	}
	return text, offset + length
}

func isPrintable(text string) bool {
	for i := 0; i < len(text); i++ {
		if !isPrintableASCII(text[i]) {
			return false
		}
	}
	return true
}

func isPrintableASCII(char byte) bool {
	return char >= 0x20 && char < 0x7F
}
