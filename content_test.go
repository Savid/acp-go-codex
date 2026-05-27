package codexacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestPromptContentMapping(t *testing.T) {
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
	input, err := promptToCodex(blocks)
	if err != nil {
		t.Fatalf("promptToCodex returned error: %v", err)
	}
	if len(input) != len(blocks) || input[2]["type"] != "mention" || input[3]["text"] != "https://example.com/doc" {
		t.Fatalf("mapped input = %#v", input)
	}
	localURI := "file:///tmp/image.png"
	localImage := acp.ImageBlock("", "image/png")
	localImage.Image.Uri = &localURI
	localInput, err := promptToCodex([]acp.ContentBlock{localImage})
	if err != nil || localInput[0]["type"] != "localImage" || localInput[0]["path"] != "/tmp/image.png" {
		t.Fatalf("local image input = %#v err=%v", localInput, err)
	}

	_, err = promptToCodex([]acp.ContentBlock{acp.AudioBlock("x", "audio/wav")})
	if err == nil {
		t.Fatal("audio block did not fail")
	}
}

func TestPromptContentImageSources(t *testing.T) {
	uri := "file:///tmp/image.png"
	input, err := promptToCodex([]acp.ContentBlock{{Image: &acp.ContentBlockImage{Uri: &uri, Type: "image"}}})
	if err != nil || input[0]["type"] != "localImage" || input[0]["path"] != "/tmp/image.png" {
		t.Fatalf("image URI input=%#v err=%v", input, err)
	}
	input, err = promptToCodex([]acp.ContentBlock{acp.ImageBlock("data", "image/png")})
	if err != nil || input[0]["url"] != "data:image/png;base64,data" {
		t.Fatalf("image data input=%#v err=%v", input, err)
	}
	if got := imageDataURL("", "abc"); got != "data:application/octet-stream;base64,abc" {
		t.Fatalf("default image data URL = %q", got)
	}
}
