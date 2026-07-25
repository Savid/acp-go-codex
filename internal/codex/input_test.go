package codex

import (
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestPromptToUserInputMapping(t *testing.T) {
	uri := "https://example.com/image.png"
	image := acp.ImageBlock("aW1hZ2U=", "image/png")
	image.Image.Uri = &uri
	blocks := []acp.ContentBlock{
		acp.TextBlock("hello"),
		image,
		acp.ResourceLinkBlock("a.go", "file:///tmp/a.go"),
		acp.ResourceLinkBlock("doc", "https://example.com/doc"),
		acp.ResourceBlock(acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{Uri: "file:///tmp/b.txt", Text: "body"}}),
		acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{Uri: "blob://id"}}),
		acp.ResourceBlock(acp.EmbeddedResourceResource{}),
	}
	input, err := PromptToUserInput(blocks, []PromptImage{{LocalPath: "/scratch/image-0.png"}})
	if err != nil {
		t.Fatalf("PromptToUserInput returned error: %v", err)
	}
	if len(input) != len(blocks) || input[2]["type"] != "mention" || input[3]["text"] != "https://example.com/doc" {
		t.Fatalf("mapped input = %#v", input)
	}
	if input[1]["type"] != "localImage" || input[1]["path"] != "/scratch/image-0.png" {
		t.Fatalf("image input = %#v, want materialized local image", input[1])
	}

	_, err = PromptToUserInput([]acp.ContentBlock{acp.AudioBlock("x", "audio/wav")}, nil)
	if !errors.Is(err, ErrAudioUnsupported) {
		t.Fatalf("audio block error = %v, want ErrAudioUnsupported", err)
	}
}

func TestPromptToUserInputImageTransport(t *testing.T) {
	input, err := PromptToUserInput([]acp.ContentBlock{acp.ImageBlock("data", "image/png")}, []PromptImage{{DataURL: "data:image/png;base64,data"}})
	if err != nil || input[0]["type"] != "image" || input[0]["url"] != "data:image/png;base64,data" {
		t.Fatalf("image data URL input=%#v err=%v", input, err)
	}

	uri := "file:///tmp/original.png"
	image := acp.ImageBlock("data", "image/png")
	image.Image.Uri = &uri
	input, err = PromptToUserInput([]acp.ContentBlock{image}, []PromptImage{{LocalPath: "/scratch/copy.png"}})
	if err != nil || input[0]["type"] != "localImage" || input[0]["path"] != "/scratch/copy.png" {
		t.Fatalf("image with URI input=%#v err=%v, want materialized path over URI", input, err)
	}
}

func TestPromptToUserInputEmbeddedImageResource(t *testing.T) {
	mimeType := "image/png"
	input, err := PromptToUserInput([]acp.ContentBlock{acp.ResourceBlock(acp.EmbeddedResourceResource{
		BlobResourceContents: &acp.BlobResourceContents{Uri: "blob://image", MimeType: &mimeType, Blob: "aW1hZ2U="},
	})}, []PromptImage{{LocalPath: "/scratch/image-0.png"}})
	if err != nil {
		t.Fatalf("PromptToUserInput returned error: %v", err)
	}
	if len(input) != 1 || input[0]["type"] != "localImage" || input[0]["path"] != "/scratch/image-0.png" {
		t.Fatalf("embedded image input = %#v", input)
	}
}

func TestPromptToUserInputImageCountMismatch(t *testing.T) {
	if _, err := PromptToUserInput([]acp.ContentBlock{acp.ImageBlock("data", "image/png")}, nil); !errors.Is(err, ErrImageNotMaterialized) {
		t.Fatalf("missing materialized image error = %v, want ErrImageNotMaterialized", err)
	}
	if _, err := PromptToUserInput([]acp.ContentBlock{acp.ImageBlock("data", "image/png")}, []PromptImage{{}}); !errors.Is(err, ErrImageNotMaterialized) {
		t.Fatalf("empty materialized image error = %v, want ErrImageNotMaterialized", err)
	}
	if _, err := PromptToUserInput([]acp.ContentBlock{acp.TextBlock("hi")}, []PromptImage{{LocalPath: "/scratch/extra.png"}}); !errors.Is(err, ErrImageNotMaterialized) {
		t.Fatalf("extra materialized image error = %v, want ErrImageNotMaterialized", err)
	}
}

func TestPromptToUserInputFailsClosedOnUnknownBlocks(t *testing.T) {
	if _, err := PromptToUserInput([]acp.ContentBlock{{}}, nil); !errors.Is(err, ErrUnsupportedContentBlock) {
		t.Fatalf("empty block error = %v, want ErrUnsupportedContentBlock", err)
	}
}

func TestIsImageBearingBlock(t *testing.T) {
	mimeType := "image/png"
	textMIME := "text/plain"
	upperMIME := "IMAGE/PNG"
	paramMIME := "image/png; charset=binary"

	cases := []struct {
		name  string
		block acp.ContentBlock
		want  bool
	}{
		{name: "image block", block: acp.ImageBlock("data", "image/png"), want: true},
		{name: "text block", block: acp.TextBlock("hi"), want: false},
		{name: "image blob resource", block: acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{Uri: "blob://a", MimeType: &mimeType, Blob: "aW1hZ2U="}}), want: true},
		{name: "non-image blob resource", block: acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{Uri: "blob://b", MimeType: &textMIME, Blob: "aW1hZ2U="}}), want: false},
		{name: "blob resource without media type", block: acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{Uri: "blob://c", Blob: "aW1hZ2U="}}), want: false},
		{name: "text resource", block: acp.ResourceBlock(acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{Uri: "file:///a", Text: "body"}}), want: false},
		{name: "uppercase image blob resource", block: acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{Uri: "blob://d", MimeType: &upperMIME, Blob: "aW1hZ2U="}}), want: true},
		{name: "parameterized image blob resource", block: acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{Uri: "blob://e", MimeType: &paramMIME, Blob: "aW1hZ2U="}}), want: true},
	}

	for _, testCase := range cases {
		if got := IsImageBearingBlock(testCase.block); got != testCase.want {
			t.Fatalf("%s: IsImageBearingBlock = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

func TestNormalizedMediaTypeRouting(t *testing.T) {
	cases := []struct {
		declared   string
		normalized string
		image      bool
	}{
		{declared: "image/png", normalized: "image/png", image: true},
		{declared: "IMAGE/PNG", normalized: "image/png", image: true},
		{declared: "Image/Png", normalized: "image/png", image: true},
		{declared: "  image/png  ", normalized: "image/png", image: true},
		{declared: "image/png;charset=binary", normalized: "image/png", image: true},
		{declared: "IMAGE/PNG; q=1", normalized: "image/png", image: true},
		{declared: "image/", normalized: "image/", image: true},
		{declared: "", normalized: "", image: false},
		{declared: "text/plain", normalized: "text/plain", image: false},
		{declared: "application/pdf", normalized: "application/pdf", image: false},
		{declared: "ximage/png", normalized: "ximage/png", image: false},
	}

	for _, testCase := range cases {
		if got := NormalizedMediaType(testCase.declared); got != testCase.normalized {
			t.Fatalf("NormalizedMediaType(%q) = %q, want %q", testCase.declared, got, testCase.normalized)
		}

		if got := IsImageMediaType(testCase.declared); got != testCase.image {
			t.Fatalf("IsImageMediaType(%q) = %v, want %v", testCase.declared, got, testCase.image)
		}
	}
}
