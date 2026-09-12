package c2pa

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// cborText encodes one definite-length CBOR text string, which is all the
// manifest fields this extractor reads.
func cborText(value string) []byte {
	out := []byte{}
	switch length := len(value); {
	case length < 24:
		out = append(out, byte(0x60|length))
	case length < 256:
		out = append(out, 0x78, byte(length))
	default:
		header := []byte{0x79, 0, 0}
		binary.BigEndian.PutUint16(header[1:], uint16(length))
		out = append(out, header...)
	}
	return append(out, value...)
}

// pngWithManifest builds the smallest PNG that carries a caBX chunk, so the
// chunk walk is exercised rather than a raw byte search.
func pngWithManifest(manifest []byte) []byte {
	out := bytes.NewBuffer(pngSignature)

	writeChunk := func(chunkType string, data []byte) {
		length := make([]byte, 4)
		binary.BigEndian.PutUint32(length, uint32(len(data)))
		out.Write(length)
		out.WriteString(chunkType)
		out.Write(data)
		out.Write([]byte{0, 0, 0, 0}) // CRC is not validated by the extractor
	}

	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], 1024)
	binary.BigEndian.PutUint32(ihdr[4:8], 768)
	writeChunk("IHDR", ihdr)
	writeChunk("caBX", manifest)
	writeChunk("IDAT", []byte{0x00})
	return out.Bytes()
}

// openAIStyleManifest mirrors the layout a real OpenAI image carries: JUMBF box
// headers with their binary lengths and UUID, then the CBOR assertion strings.
// The binary preamble matters — it is what the string scanner has to walk past
// without mistaking a length byte for a text header.
func openAIStyleManifest() []byte {
	manifest := []byte{0x00, 0x00, 0x00, 0x55}
	manifest = append(manifest, "jumb"...)
	manifest = append(manifest, 0x00, 0x00, 0x00, 0x47)
	manifest = append(manifest, "jumd"...)
	manifest = append(manifest, make([]byte, 16)...) // box UUID
	manifest = append(manifest, "c2pa"...)
	for _, token := range []string{
		"c2pa.assertions",
		"c2pa.actions",
		"action", "c2pa.created",
		// softwareAgent holds an object, so "name" follows it before the value.
		// Reading the key that follows as the generator is the bug this guards.
		"softwareAgent", "name", "gpt-image",
		"version", "2.0",
		"digitalSourceType", "trainedAlgorithmicMedia",
	} {
		manifest = append(manifest, cborText(token)...)
	}
	// A DER-style length prefix in front of the signing organization, which is
	// how the issuer is bounded in the certificate chain.
	manifest = append(manifest, byte(len("OpenAI Media Service API")))
	manifest = append(manifest, "OpenAI Media Service API"...)
	manifest = append(manifest, "dicon"...) // the token that follows it in a real file
	return manifest
}

// The whole reason this package exists: the Codex image endpoint answers a
// gpt-image-2.5-flare request with a 2.0 image, and the manifest is the only
// place that shows it.
func TestExtractReportsTheRealGeneratorVersion(t *testing.T) {
	summary := Extract(pngWithManifest(openAIStyleManifest()))
	if !summary.Present {
		t.Fatal("a PNG with a caBX chunk must report a manifest")
	}
	if summary.Generator != "gpt-image" {
		t.Fatalf("generator = %q, want gpt-image", summary.Generator)
	}
	if summary.GeneratorVersion != "2.0" {
		t.Fatalf("version = %q, want 2.0", summary.GeneratorVersion)
	}
	if summary.DigitalSourceType != "trainedAlgorithmicMedia" {
		t.Fatalf("digital source = %q", summary.DigitalSourceType)
	}
	if len(summary.Actions) == 0 {
		t.Fatal("c2pa.created must be reported as an action")
	}
	if len(summary.Fields) == 0 {
		t.Fatal("recovered fields must stay available as the operator's escape hatch")
	}
	// The issuer must stop at its length prefix instead of running into the next
	// token, which produced "OpenAI Media Service APIdicon".
	if summary.Issuer != "OpenAI Media Service API" {
		t.Fatalf("issuer = %q, want it bounded by its length prefix", summary.Issuer)
	}
}

func TestExtractReportsNothingForAnUnsignedAsset(t *testing.T) {
	plain := pngWithManifest(nil)
	// Rebuild without the caBX chunk entirely.
	plain = bytes.Replace(plain, []byte("caBX"), []byte("tEXt"), 1)
	if summary := Extract(plain); summary.Present {
		t.Fatal("an asset with no manifest must not claim provenance")
	}
	if summary := Extract(nil); summary.Present {
		t.Fatal("empty input must not claim provenance")
	}
	if summary := Extract([]byte("not an image at all")); summary.Present {
		t.Fatal("arbitrary bytes must not be read as a manifest")
	}
}

// Pixel data must not be mistaken for a manifest, and the walk must stop at
// IDAT rather than scanning megabytes of scanlines.
func TestPNGManifestStopsAtPixelData(t *testing.T) {
	out := bytes.NewBuffer(pngSignature)
	writeChunk := func(chunkType string, data []byte) {
		length := make([]byte, 4)
		binary.BigEndian.PutUint32(length, uint32(len(data)))
		out.Write(length)
		out.WriteString(chunkType)
		out.Write(data)
		out.Write([]byte{0, 0, 0, 0})
	}
	writeChunk("IHDR", make([]byte, 13))
	writeChunk("IDAT", []byte("caBX jumb c2pa claim_generator_info"))
	if block := pngManifest(out.Bytes()); block != nil {
		t.Fatal("bytes inside IDAT must not be read as a manifest")
	}
}

func TestReadCBORTextRejectsBinaryNoise(t *testing.T) {
	// 0x62 is text(2) but the payload is not printable ASCII.
	if text, size := readCBORText([]byte{0x62, 0x00, 0x01}); size != 0 {
		t.Fatalf("decoded %q from binary noise", text)
	}
	// Single characters are coincidence in binary data far more often than they
	// are manifest values.
	if _, size := readCBORText([]byte{0x61, 'a'}); size != 0 {
		t.Fatal("one-character strings must not be collected")
	}
	text, size := readCBORText(append([]byte{0x64}, "name"...))
	if size != 5 || text != "name" {
		t.Fatalf("text = %q size = %d, want name/5", text, size)
	}
}
