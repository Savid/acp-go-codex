package codex

import (
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestPromptToUserInputMapping(t *testing.T) {
	uri := "https://example.com/image.png"
	image := acp.ImageBlock("", "image/png")
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
	input, err := PromptToUserInput(blocks)
	if err != nil {
		t.Fatalf("PromptToUserInput returned error: %v", err)
	}
	if len(input) != len(blocks) || input[2]["type"] != "mention" || input[3]["text"] != "https://example.com/doc" {
		t.Fatalf("mapped input = %#v", input)
	}
	localURI := "file:///tmp/image.png"
	localImage := acp.ImageBlock("", "image/png")
	localImage.Image.Uri = &localURI
	localInput, err := PromptToUserInput([]acp.ContentBlock{localImage})
	if err != nil || localInput[0]["type"] != "localImage" || localInput[0]["path"] != "/tmp/image.png" {
		t.Fatalf("local image input = %#v err=%v", localInput, err)
	}

	_, err = PromptToUserInput([]acp.ContentBlock{acp.AudioBlock("x", "audio/wav")})
	if !errors.Is(err, ErrAudioUnsupported) {
		t.Fatalf("audio block error = %v, want ErrAudioUnsupported", err)
	}
}

func TestPromptToUserInputImageSources(t *testing.T) {
	uri := "file:///tmp/image.png"
	input, err := PromptToUserInput([]acp.ContentBlock{{Image: &acp.ContentBlockImage{Uri: &uri, Type: "image"}}})
	if err != nil || input[0]["type"] != "localImage" || input[0]["path"] != "/tmp/image.png" {
		t.Fatalf("image URI input=%#v err=%v", input, err)
	}
	input, err = PromptToUserInput([]acp.ContentBlock{acp.ImageBlock("data", "image/png")})
	if err != nil || input[0]["url"] != "data:image/png;base64,data" {
		t.Fatalf("image data input=%#v err=%v", input, err)
	}
	if got := imageDataURL("", "abc"); got != "data:application/octet-stream;base64,abc" {
		t.Fatalf("default image data URL = %q", got)
	}
	if _, err := PromptToUserInput([]acp.ContentBlock{acp.ImageBlock("", "image/png")}); !errors.Is(err, ErrImageMissingData) {
		t.Fatalf("data-less image error = %v, want ErrImageMissingData", err)
	}
}

func TestPromptToUserInputEmbeddedImageResource(t *testing.T) {
	mimeType := "image/png"
	input, err := PromptToUserInput([]acp.ContentBlock{acp.ResourceBlock(acp.EmbeddedResourceResource{
		BlobResourceContents: &acp.BlobResourceContents{Uri: "blob://image", MimeType: &mimeType, Blob: "aW1hZ2U="},
	})})
	if err != nil {
		t.Fatalf("PromptToUserInput returned error: %v", err)
	}
	if len(input) != 1 || input[0]["type"] != "image" || input[0]["url"] != "data:image/png;base64,aW1hZ2U=" {
		t.Fatalf("embedded image input = %#v", input)
	}
}

func TestPromptToUserInputFailsClosedOnUnknownBlocks(t *testing.T) {
	if _, err := PromptToUserInput([]acp.ContentBlock{{}}); !errors.Is(err, ErrUnsupportedContentBlock) {
		t.Fatalf("empty block error = %v, want ErrUnsupportedContentBlock", err)
	}
}
