package codex

import (
	"errors"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

// ErrAudioUnsupported reports that a prompt carried an audio content block,
// which the Codex app-server cannot accept. Callers map it to the uniform ACP
// unsupported-content error.
var ErrAudioUnsupported = errors.New("audio prompt blocks are not supported by Codex app-server")

const (
	inputText       = "text"
	inputURL        = "url"
	inputLocalImage = "localImage"
	inputImage      = "image"
	inputMention    = "mention"
)

// PromptToUserInput maps ACP prompt content blocks into the native Codex
// app-server user-input representation. It returns ErrAudioUnsupported when a
// block carries audio.
func PromptToUserInput(blocks []acp.ContentBlock) ([]UserInput, error) {
	input := make([]UserInput, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			input = append(input, UserInput{fieldType: inputText, inputText: block.Text.Text})
		case block.Image != nil:
			if block.Image.Uri != nil && *block.Image.Uri != "" {
				if strings.HasPrefix(*block.Image.Uri, "file://") {
					input = append(input, UserInput{fieldType: inputLocalImage, fieldPath: strings.TrimPrefix(*block.Image.Uri, "file://")})
				} else {
					input = append(input, UserInput{fieldType: inputImage, inputURL: *block.Image.Uri})
				}
			} else {
				input = append(input, UserInput{
					fieldType: inputImage,
					inputURL:  imageDataURL(block.Image.MimeType, block.Image.Data),
				})
			}
		case block.ResourceLink != nil:
			input = append(input, resourceLinkInput(*block.ResourceLink))
		case block.Resource != nil:
			input = append(input, resourceInput(block.Resource.Resource))
		case block.Audio != nil:
			return nil, ErrAudioUnsupported
		}
	}

	return input, nil
}

func imageDataURL(mimeType string, data string) string {
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	return "data:" + mimeType + ";base64," + data
}

func resourceLinkInput(resource acp.ContentBlockResourceLink) UserInput {
	if strings.HasPrefix(resource.Uri, "file://") {
		return UserInput{fieldType: inputMention, fieldName: firstNonEmpty(resource.Name, resource.Uri), fieldPath: strings.TrimPrefix(resource.Uri, "file://")}
	}

	return UserInput{fieldType: inputText, inputText: resource.Uri}
}

func resourceInput(resource acp.EmbeddedResourceResource) UserInput {
	switch {
	case resource.TextResourceContents != nil:
		contents := resource.TextResourceContents

		return UserInput{fieldType: inputText, inputText: fmt.Sprintf("\n<context ref=%q>\n%s\n</context>", contents.Uri, contents.Text)}
	case resource.BlobResourceContents != nil:
		contents := resource.BlobResourceContents

		return UserInput{fieldType: inputText, inputText: fmt.Sprintf("[resource: %s]", contents.Uri)}
	default:
		return UserInput{fieldType: inputText, inputText: "[resource]"}
	}
}
